package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/config"
	"github.com/davidschrooten/orbit-machines/internal/k8s"
)

func TestDispatcherCLI_VersionAndServe(t *testing.T) {
	t.Parallel()

	t.Run("version subcommand", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"version"}, &out, &errOut, nil)
		if code != 0 {
			t.Fatalf("run(version) code = %d, stderr = %s", code, errOut.String())
		}
		if !strings.Contains(out.String(), "orbit-dispatcher") {
			t.Errorf("version output = %q, want orbit-dispatcher", out.String())
		}
	})

	t.Run("serve subcommand loads config and invokes runner", func(t *testing.T) {
		t.Parallel()
		var captured config.DispatcherConfig
		runner := func(_ context.Context, cfg config.DispatcherConfig) error {
			captured = cfg
			return nil
		}

		var out, errOut bytes.Buffer
		args := []string{
			"serve",
			"--orbit-url=https://orbit.acme.dev",
			"--cluster-token=omc_live_0123456789abcdef0123456789",
			"--ingress-host=machines.acme.dev",
			"--max-concurrent-pods=15",
		}
		code := run(context.Background(), args, &out, &errOut, runner)
		if code != 0 {
			t.Fatalf("run(serve) code = %d, stderr = %s", code, errOut.String())
		}
		if captured.OrbitURL != "https://orbit.acme.dev" || captured.MaxConcurrentPods != 15 {
			t.Errorf("unexpected captured config: %+v", captured)
		}
	})

	t.Run("serve subcommand with default runner starts and shuts down cleanly", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		args := []string{
			"serve",
			"--listen-addr=127.0.0.1:0",
			"--orbit-url=https://orbit.acme.dev",
			"--cluster-token=omc_live_0123456789abcdef0123456789",
			"--ingress-host=machines.acme.dev",
		}
		code := run(ctx, args, &out, &errOut, nil)
		if code != 0 {
			t.Fatalf("run(serve default) code = %d, stderr = %s", code, errOut.String())
		}
	})

	t.Run("serve subcommand fails on invalid config", func(t *testing.T) {
		t.Parallel()
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"serve"}, &out, &errOut, nil)
		if code != 1 {
			t.Fatalf("expected exit code 1 on missing required flags, got %d", code)
		}
	})

	t.Run("serve subcommand propagates runner error", func(t *testing.T) {
		t.Parallel()
		runner := func(_ context.Context, _ config.DispatcherConfig) error {
			return errors.New("boom")
		}
		var out, errOut bytes.Buffer
		args := []string{
			"serve",
			"--orbit-url=https://orbit.acme.dev",
			"--cluster-token=omc_live_0123456789abcdef0123456789",
			"--ingress-host=machines.acme.dev",
		}
		code := run(context.Background(), args, &out, &errOut, runner)
		if code != 1 {
			t.Fatalf("expected exit code 1 on runner error, got %d", code)
		}
	})

	t.Run("buildOrchestrator selects HTTP orchestrator in cluster and falls back out of cluster", func(t *testing.T) {
		t.Parallel()
		cfg := config.DispatcherConfig{
			KubernetesNamespace: "orbit-machines",
			MachineImage:        "ghcr.io/davidschrooten/orbit-machined:0.1.0",
			MaxConcurrentPods:   10,
		}
		if _, ok := buildOrchestrator(cfg, "", "", "", "").(*k8s.FakePodOrchestrator); !ok {
			t.Fatalf("expected FakePodOrchestrator when KUBERNETES_SERVICE_HOST is empty")
		}

		dir := t.TempDir()
		tokFile := filepath.Join(dir, "token")
		caFile := filepath.Join(dir, "ca.crt")
		_ = os.WriteFile(tokFile, []byte("sa-token-xyz\n"), 0o600)
		_ = os.WriteFile(caFile, generateSelfSignedCAPEM(t), 0o600)

		if _, ok := buildOrchestrator(cfg, "10.43.0.1", "", tokFile, caFile).(*k8s.HTTPPodOrchestrator); !ok {
			t.Fatalf("expected HTTPPodOrchestrator when KUBERNETES_SERVICE_HOST is set")
		}
	})
}

func generateSelfSignedCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
