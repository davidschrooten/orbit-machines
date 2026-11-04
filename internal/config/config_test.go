package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/config"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func TestLoadDispatcher_PrecedenceAndDefaults(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "dispatcher.yaml")
	yamlContent := `
listen_addr: ":9090"
orbit_url: "https://orbit.from-file.example.com"
cluster_token: "omc_live_from_file_0123456789abcdef"
ingress_host: "machines.from-file.example.com"
max_concurrent_pods: 40
`
	if err := os.WriteFile(cfgPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	// Env var overrides file value for orbit_url
	t.Setenv("ORBIT_DISPATCHER_ORBIT_URL", "https://orbit.from-env.example.com")

	v := viper.New()
	fs := pflag.NewFlagSet("dispatcher", pflag.ContinueOnError)
	if err := config.BindDispatcherFlags(v, fs); err != nil {
		t.Fatalf("BindDispatcherFlags error: %v", err)
	}

	// CLI flag overrides both env and file for max-concurrent-pods
	if err := fs.Parse([]string{"--max-concurrent-pods=55"}); err != nil {
		t.Fatalf("fs.Parse error: %v", err)
	}

	cfg, err := config.LoadDispatcher(v, cfgPath)
	if err != nil {
		t.Fatalf("LoadDispatcher unexpected error: %v", err)
	}

	if cfg.ListenAddr != ":9090" {
		t.Errorf("ListenAddr = %q, want :9090 (from file)", cfg.ListenAddr)
	}
	if cfg.OrbitURL != "https://orbit.from-env.example.com" {
		t.Errorf("OrbitURL = %q, want env override", cfg.OrbitURL)
	}
	if cfg.MaxConcurrentPods != 55 {
		t.Errorf("MaxConcurrentPods = %d, want 55 (from CLI flag)", cfg.MaxConcurrentPods)
	}
	if cfg.KubernetesNamespace != "orbit-machines" {
		t.Errorf("KubernetesNamespace = %q, want default orbit-machines", cfg.KubernetesNamespace)
	}
	if cfg.HeartbeatInterval != 30*time.Second {
		t.Errorf("HeartbeatInterval = %v, want 30s default", cfg.HeartbeatInterval)
	}
}

func TestLoadDispatcher_ValidationErrors(t *testing.T) {
	t.Parallel()

	baseArgs := func(overrides ...string) []string {
		args := []string{
			"--orbit-url=https://orbit.example.com",
			"--cluster-token=omc_live_0123456789abcdef0123456789",
			"--ingress-host=machines.example.com",
		}
		return append(args, overrides...)
	}

	cases := []struct {
		name       string
		args       []string
		configFile string
		wantSubstr string
	}{
		{
			name:       "non-existent config file",
			args:       baseArgs(),
			configFile: "/nonexistent/path/dispatcher.yaml",
			wantSubstr: "reading config file",
		},
		{
			name:       "invalid orbit URL scheme",
			args:       baseArgs("--orbit-url=ftp://orbit.example.com"),
			wantSubstr: "orbit_url",
		},
		{
			name:       "missing orbit URL host",
			args:       baseArgs("--orbit-url=https://"),
			wantSubstr: "orbit_url",
		},
		{
			name:       "short cluster token",
			args:       baseArgs("--cluster-token=short"),
			wantSubstr: "cluster_token",
		},
		{
			name:       "empty ingress host",
			args:       baseArgs("--ingress-host="),
			wantSubstr: "ingress_host",
		},
		{
			name:       "empty listen addr",
			args:       baseArgs("--listen-addr="),
			wantSubstr: "listen_addr",
		},
		{
			name:       "zero max concurrent pods",
			args:       baseArgs("--max-concurrent-pods=0"),
			wantSubstr: "max_concurrent_pods",
		},
		{
			name:       "zero heartbeat interval",
			args:       baseArgs("--heartbeat-interval=0s"),
			wantSubstr: "heartbeat_interval",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := viper.New()
			fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
			if err := config.BindDispatcherFlags(v, fs); err != nil {
				t.Fatalf("BindDispatcherFlags error: %v", err)
			}
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("fs.Parse error: %v", err)
			}
			_, err := config.LoadDispatcher(v, tc.configFile)
			if !errors.Is(err, domain.ErrInvalidConfig) {
				t.Fatalf("LoadDispatcher err = %v, want wrapping ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestLoadMachined_ValidAndValidationErrors(t *testing.T) {
	t.Parallel()

	validKeyHex := strings.Repeat("ab", 32) // 64 hex chars = 32 bytes

	t.Run("valid configuration with S3 and 32-byte hex key", func(t *testing.T) {
		t.Parallel()
		v := viper.New()
		fs := pflag.NewFlagSet("machined", pflag.ContinueOnError)
		if err := config.BindMachinedFlags(v, fs); err != nil {
			t.Fatalf("BindMachinedFlags error: %v", err)
		}
		err := fs.Parse([]string{
			"--workspace-id=ws_99",
			"--user-id=usr_42",
			"--resource-tier=performance",
			"--s3-endpoint=http://minio:9000",
			"--profile-encryption-key=" + validKeyHex,
		})
		if err != nil {
			t.Fatalf("fs.Parse error: %v", err)
		}

		cfg, err := config.LoadMachined(v, "")
		if err != nil {
			t.Fatalf("LoadMachined error: %v", err)
		}
		if cfg.MachineID.PodName() != "machine-ws-99-usr-42" {
			t.Errorf("PodName = %q, want machine-ws-99-usr-42", cfg.MachineID.PodName())
		}
		if cfg.ResourceTier != domain.TierPerformance {
			t.Errorf("ResourceTier = %q, want performance", cfg.ResourceTier)
		}
		if len(cfg.ProfileEncryptionKey) != 32 {
			t.Errorf("ProfileEncryptionKey len = %d, want 32", len(cfg.ProfileEncryptionKey))
		}
	})

	cases := []struct {
		name       string
		args       []string
		configFile string
		wantSubstr string
	}{
		{
			name:       "non-existent config file",
			args:       []string{"--workspace-id=ws_1", "--user-id=usr_1"},
			configFile: "/nonexistent/machined.yaml",
			wantSubstr: "reading config file",
		},
		{
			name:       "missing workspace ID",
			args:       []string{"--user-id=usr_1"},
			wantSubstr: "invalid machine identifier",
		},
		{
			name:       "invalid resource tier",
			args:       []string{"--workspace-id=ws_1", "--user-id=usr_1", "--resource-tier=huge"},
			wantSubstr: "invalid resource tier",
		},
		{
			name:       "empty listen addr",
			args:       []string{"--workspace-id=ws_1", "--user-id=usr_1", "--listen-addr="},
			wantSubstr: "listen_addr",
		},
		{
			name:       "zero idle timeout",
			args:       []string{"--workspace-id=ws_1", "--user-id=usr_1", "--idle-timeout=0s"},
			wantSubstr: "idle_timeout",
		},
		{
			name:       "invalid hex in encryption key",
			args:       []string{"--workspace-id=ws_1", "--user-id=usr_1", "--profile-encryption-key=not-hex"},
			wantSubstr: "profile_encryption_key",
		},
		{
			name:       "wrong byte length encryption key when S3 enabled",
			args:       []string{"--workspace-id=ws_1", "--user-id=usr_1", "--s3-endpoint=http://minio:9000", "--profile-encryption-key=abcd"},
			wantSubstr: "profile_encryption_key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := viper.New()
			fs := pflag.NewFlagSet("machined-err", pflag.ContinueOnError)
			if err := config.BindMachinedFlags(v, fs); err != nil {
				t.Fatalf("BindMachinedFlags error: %v", err)
			}
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("fs.Parse error: %v", err)
			}
			_, err := config.LoadMachined(v, tc.configFile)
			if !errors.Is(err, domain.ErrInvalidConfig) {
				t.Fatalf("LoadMachined err = %v, want wrapping ErrInvalidConfig", err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
