// Command orbit-dispatcher is the Kubernetes cluster gateway and Pod reconciler
// for Orbit Machines.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/config"
	"github.com/davidschrooten/orbit-machines/internal/dispatcher"
	"github.com/davidschrooten/orbit-machines/internal/k8s"
	"github.com/davidschrooten/orbit-machines/internal/orbitclient"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/ticket"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Version is the semantic version of orbit-dispatcher.
const Version = "0.1.0"

// ServeRunner executes the dispatcher gateway with a validated configuration.
type ServeRunner func(ctx context.Context, cfg config.DispatcherConfig) error

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr, nil))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, runner ServeRunner) int {
	cmd := NewRootCmd(stdout, stderr, runner)
	cmd.SetArgs(args)
	if err := cmd.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

// NewRootCmd constructs the Cobra command tree for orbit-dispatcher without global state.
func NewRootCmd(stdout, stderr io.Writer, runner ServeRunner) *cobra.Command {
	if runner == nil {
		runner = defaultServeRunner(stderr)
	}

	var configFile string
	rootCmd := &cobra.Command{
		Use:           "orbit-dispatcher",
		Short:         "Orbit Machines Kubernetes cluster gateway and Pod reconciler",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)
	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "", "Path to configuration file (YAML/TOML)")

	v := viper.New()
	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the HTTP/WebSocket gateway and Kubernetes Pod reconciler",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadDispatcher(v, configFile)
			if err != nil {
				return err
			}
			return runner(cmd.Context(), cfg)
		},
	}
	_ = config.BindDispatcherFlags(v, serveCmd.Flags())

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the orbit-dispatcher version",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "orbit-dispatcher v%s\n", Version)
		},
	}

	rootCmd.AddCommand(serveCmd, versionCmd)
	return rootCmd
}

func defaultServeRunner(logOut io.Writer) ServeRunner {
	return func(ctx context.Context, cfg config.DispatcherConfig) error {
		logger := slog.New(slog.NewJSONHandler(logOut, nil))
		ticketCodec, _ := ticket.NewCodecFromClusterToken(cfg.ClusterToken)
		frameCodec, _ := protocol.NewFrameCodec()
		orbitClient, _ := orbitclient.NewClient(cfg.OrbitURL, cfg.ClusterToken)
		orchestrator := buildOrchestrator(cfg, os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"),
			"/var/run/secrets/kubernetes.io/serviceaccount/token",
			"/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		)

		srv, err := dispatcher.NewServer(dispatcher.ServerConfig{
			OrbitURL:          cfg.OrbitURL,
			ClusterToken:      cfg.ClusterToken,
			MaxConcurrentPods: cfg.MaxConcurrentPods,
			TicketCodec:       ticketCodec,
			FrameCodec:        frameCodec,
			Orchestrator:      orchestrator,
			OrbitClient:       orbitClient,
		})
		if err != nil {
			return err
		}

		ln, err := net.Listen("tcp", cfg.ListenAddr)
		if err != nil {
			return err
		}
		httpSrv := &http.Server{
			Handler:           srv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}

		logger.InfoContext(ctx, "orbit-dispatcher listening",
			slog.String("listen_addr", ln.Addr().String()),
			slog.String("namespace", cfg.KubernetesNamespace),
			slog.String("ingress_host", cfg.IngressHost),
		)

		go func() {
			if _, regErr := orbitClient.RegisterCluster(ctx, protocol.RegisterClusterRequest{
				ClusterVersion:      "v" + Version,
				DispatcherURL:       "https://" + cfg.IngressHost,
				IngressHost:         cfg.IngressHost,
				KubernetesNamespace: cfg.KubernetesNamespace,
				MinIOEnabled:        cfg.S3Endpoint != "",
				MaxConcurrentPods:   cfg.MaxConcurrentPods,
			}); regErr != nil {
				logger.WarnContext(ctx, "initial cluster registration failed", slog.String("error", regErr.Error()))
			}
			_ = srv.RunHeartbeatLoop(ctx, cfg.HeartbeatInterval)
		}()

		errCh := make(chan error, 1)
		go func() {
			if serveErr := httpSrv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				errCh <- serveErr
				return
			}
			errCh <- nil
		}()

		select {
		case <-ctx.Done():
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutCtx)
			return <-errCh
		case serveErr := <-errCh:
			return serveErr
		}
	}
}

func buildOrchestrator(cfg config.DispatcherConfig, k8sHost, k8sPort, tokenPath, caPath string) k8s.PodOrchestrator {
	host := strings.TrimSpace(k8sHost)
	if host == "" {
		return k8s.NewFakePodOrchestrator(cfg.MaxConcurrentPods)
	}
	port := strings.TrimSpace(k8sPort)
	if port == "" {
		port = "443"
	}
	tokenBytes, _ := os.ReadFile(tokenPath)
	caBytes, _ := os.ReadFile(caPath)

	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caBytes) > 0 {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caBytes) {
			tlsCfg.RootCAs = pool
		}
	}

	orch, err := k8s.NewHTTPPodOrchestrator(k8s.HTTPConfig{
		APIServerURL: "https://" + net.JoinHostPort(host, port),
		BearerToken:  strings.TrimSpace(string(tokenBytes)),
		Manifest: k8s.ManifestConfig{
			Namespace:        cfg.KubernetesNamespace,
			MachineImage:     cfg.MachineImage,
			S3Endpoint:       cfg.S3Endpoint,
			S3ProfileBucket:  cfg.S3ProfileBucket,
			S3NixCacheBucket: cfg.S3NixCacheBucket,
		},
		MaxConcurrentPods: cfg.MaxConcurrentPods,
		HTTPClient: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	})
	if err != nil {
		return k8s.NewFakePodOrchestrator(cfg.MaxConcurrentPods)
	}
	return orch
}
