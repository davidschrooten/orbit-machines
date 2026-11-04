package helmchart

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const productionChartDir = "../../charts/orbit-machines"

func TestValidateProductionChart(t *testing.T) {
	t.Parallel()

	report, err := InspectChart(productionChartDir)
	if err != nil {
		t.Fatalf("InspectChart(%q) unexpected error: %v", productionChartDir, err)
	}
	if report.Metadata.Name != "orbit-machines" {
		t.Fatalf("Metadata.Name = %q, want orbit-machines", report.Metadata.Name)
	}
	if report.Metadata.Version != "0.1.0" {
		t.Fatalf("Metadata.Version = %q, want 0.1.0", report.Metadata.Version)
	}

	if err := ValidateChart(productionChartDir); err != nil {
		t.Fatalf("ValidateChart(%q) unexpected error: %v", productionChartDir, err)
	}
}

func TestInspectChartErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing Chart.yaml", func(t *testing.T) {
		t.Parallel()
		_, err := InspectChart(t.TempDir())
		if !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("invalid Chart.yaml YAML", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(":::invalid"), 0o600)
		_, err := InspectChart(dir)
		if !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("missing values.yaml", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: orbit-machines\n"), 0o600)
		_, err := InspectChart(dir)
		if !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("invalid values.yaml YAML", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: orbit-machines\n"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(":::invalid"), 0o600)
		_, err := InspectChart(dir)
		if !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("missing template file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte("apiVersion: v2\nname: orbit-machines\n"), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("dispatcher:\n  replicaCount: 1\n"), 0o600)
		if err := ValidateChart(dir); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})
}

func TestValidateMetadataInvariants(t *testing.T) {
	t.Parallel()

	base, err := InspectChart(productionChartDir)
	if err != nil {
		t.Fatalf("InspectChart: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*ChartMetadata)
	}{
		{
			name:   "wrong apiVersion",
			mutate: func(m *ChartMetadata) { m.APIVersion = "v1" },
		},
		{
			name:   "wrong chart name",
			mutate: func(m *ChartMetadata) { m.Name = "other-chart" },
		},
		{
			name:   "missing version",
			mutate: func(m *ChartMetadata) { m.Version = "" },
		},
		{
			name:   "missing description",
			mutate: func(m *ChartMetadata) { m.Description = "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta := base.Metadata
			tc.mutate(&meta)
			if err := validateMetadata(meta); !errors.Is(err, ErrInvalidChart) {
				t.Fatalf("validateMetadata() = %v, want ErrInvalidChart", err)
			}
		})
	}
}

func TestValidateValuesInvariants(t *testing.T) {
	t.Parallel()

	base, err := InspectChart(productionChartDir)
	if err != nil {
		t.Fatalf("InspectChart: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Values)
	}{
		{
			name:   "zero replicas",
			mutate: func(v *Values) { v.Dispatcher.ReplicaCount = 0 },
		},
		{
			name:   "wrong dispatcher repository",
			mutate: func(v *Values) { v.Dispatcher.Image.Repository = "example.com/bad" },
		},
		{
			name:   "wrong dispatcher port",
			mutate: func(v *Values) { v.Dispatcher.Service.Port = 9090 },
		},
		{
			name:   "empty idleTimeout",
			mutate: func(v *Values) { v.Dispatcher.IdleTimeout = "" },
		},
		{
			name:   "wrong machined repository",
			mutate: func(v *Values) { v.Machined.Image.Repository = "example.com/bad" },
		},
		{
			name:   "empty machined defaultCPU",
			mutate: func(v *Values) { v.Machined.DefaultCPU = "" },
		},
		{
			name:   "empty orbit url",
			mutate: func(v *Values) { v.Orbit.URL = "" },
		},
		{
			name:   "wrong s3 vaultBucket",
			mutate: func(v *Values) { v.S3.VaultBucket = "wrong" },
		},
		{
			name:   "disabled networkPolicy",
			mutate: func(v *Values) { v.NetworkPolicy.Enabled = false },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			vals := base.Values
			tc.mutate(&vals)
			if err := validateValues(vals); !errors.Is(err, ErrInvalidChart) {
				t.Fatalf("validateValues() = %v, want ErrInvalidChart", err)
			}
		})
	}
}

func TestValidateTemplatesInvariants(t *testing.T) {
	t.Parallel()

	base, err := InspectChart(productionChartDir)
	if err != nil {
		t.Fatalf("InspectChart: %v", err)
	}

	cloneTpls := func() map[string]string {
		out := make(map[string]string, len(base.Templates))
		for k, v := range base.Templates {
			out[k] = v
		}
		return out
	}

	t.Run("missing probe in deployment.yaml", func(t *testing.T) {
		t.Parallel()
		tpls := cloneTpls()
		tpls["deployment.yaml"] = strings.ReplaceAll(tpls["deployment.yaml"], "/readyz", "/missing")
		if err := validateTemplates(tpls); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("missing env var in deployment.yaml", func(t *testing.T) {
		t.Parallel()
		tpls := cloneTpls()
		tpls["deployment.yaml"] = strings.ReplaceAll(tpls["deployment.yaml"], "ORBIT_MACHINES_CLUSTER_TOKEN", "REMOVED_VAR")
		if err := validateTemplates(tpls); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("ClusterRole forbidden in rbac.yaml", func(t *testing.T) {
		t.Parallel()
		tpls := cloneTpls()
		tpls["rbac.yaml"] += "\nkind: ClusterRole\n"
		if err := validateTemplates(tpls); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("missing pods/log in rbac.yaml", func(t *testing.T) {
		t.Parallel()
		tpls := cloneTpls()
		tpls["rbac.yaml"] = strings.ReplaceAll(tpls["rbac.yaml"], `"pods/log"`, `"other"`)
		if err := validateTemplates(tpls); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("missing metadata CIDR block in networkpolicy.yaml", func(t *testing.T) {
		t.Parallel()
		tpls := cloneTpls()
		tpls["networkpolicy.yaml"] = strings.ReplaceAll(tpls["networkpolicy.yaml"], "169.254.169.254/32", "")
		if err := validateTemplates(tpls); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})

	t.Run("missing bucket init in minio-init-job.yaml", func(t *testing.T) {
		t.Parallel()
		tpls := cloneTpls()
		tpls["minio-init-job.yaml"] = strings.ReplaceAll(tpls["minio-init-job.yaml"], "vaultBucket", "")
		if err := validateTemplates(tpls); !errors.Is(err, ErrInvalidChart) {
			t.Fatalf("err = %v, want ErrInvalidChart", err)
		}
	})
}

func TestValidateChartEndToEndFailures(t *testing.T) {
	t.Parallel()

	base, err := InspectChart(productionChartDir)
	if err != nil {
		t.Fatalf("InspectChart: %v", err)
	}

	writeChart := func(t *testing.T, chartYAML, valuesYAML string) string {
		t.Helper()
		dir := t.TempDir()
		_ = os.MkdirAll(filepath.Join(dir, "templates"), 0o755)
		_ = os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(chartYAML), 0o600)
		_ = os.WriteFile(filepath.Join(dir, "values.yaml"), []byte(valuesYAML), 0o600)
		for name, content := range base.Templates {
			_ = os.WriteFile(filepath.Join(dir, "templates", name), []byte(content), 0o600)
		}
		return dir
	}

	validValuesBytes, _ := os.ReadFile(filepath.Join(productionChartDir, "values.yaml"))
	validChartBytes, _ := os.ReadFile(filepath.Join(productionChartDir, "Chart.yaml"))

	badMetaDir := writeChart(t, "apiVersion: v1\nname: bad\n", string(validValuesBytes))
	if err := ValidateChart(badMetaDir); !errors.Is(err, ErrInvalidChart) {
		t.Fatalf("ValidateChart(badMetaDir) = %v, want ErrInvalidChart", err)
	}

	badValuesDir := writeChart(t, string(validChartBytes), "dispatcher:\n  replicaCount: 0\n")
	if err := ValidateChart(badValuesDir); !errors.Is(err, ErrInvalidChart) {
		t.Fatalf("ValidateChart(badValuesDir) = %v, want ErrInvalidChart", err)
	}
}

func TestHelmLintAndTemplateIntegration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	// Always test error handling with a non-existent chart path.
	if _, err := LintWithHelm(ctx, "", filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("LintWithHelm on nonexistent dir succeeded, want error")
	}
	if _, err := RenderWithHelm(ctx, "", filepath.Join(t.TempDir(), "nonexistent"), map[string]string{"a": "b"}); err == nil {
		t.Fatal("RenderWithHelm on nonexistent dir succeeded, want error")
	}

	helmBin, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm binary not in PATH; skipping live helm CLI execution")
	}

	lintOut, err := LintWithHelm(ctx, helmBin, productionChartDir)
	if err != nil {
		t.Fatalf("LintWithHelm failed: %v\n%s", err, lintOut)
	}
	if !strings.Contains(lintOut, "0 chart(s) failed") {
		t.Fatalf("unexpected helm lint output: %s", lintOut)
	}

	renderedDefault, err := RenderWithHelm(ctx, helmBin, productionChartDir, nil)
	if err != nil {
		t.Fatalf("RenderWithHelm(default) failed: %v", err)
	}
	for _, want := range []string{
		"kind: ServiceAccount",
		"kind: Role",
		"kind: RoleBinding",
		"kind: Secret",
		"kind: Deployment",
		"kind: Service",
		"kind: NetworkPolicy",
		"kind: StatefulSet",
		"kind: Job",
		"ORBIT_MACHINES_CLUSTER_TOKEN",
		"http://orbit-machines-minio:9000",
	} {
		if !strings.Contains(renderedDefault, want) {
			t.Errorf("rendered default manifests missing %q", want)
		}
	}

	renderedCustom, err := RenderWithHelm(ctx, helmBin, productionChartDir, map[string]string{
		"dispatcher.ingress.enabled": "true",
		"orbit.existingSecret":       "external-cluster-secret",
		"s3.minio.enabled":           "false",
		"s3.endpoint":                "https://s3.eu-central-1.amazonaws.com",
	})
	if err != nil {
		t.Fatalf("RenderWithHelm(custom) failed: %v", err)
	}
	for _, want := range []string{
		"kind: Ingress",
		"name: external-cluster-secret",
		"https://s3.eu-central-1.amazonaws.com",
	} {
		if !strings.Contains(renderedCustom, want) {
			t.Errorf("rendered custom manifests missing %q", want)
		}
	}
	if strings.Contains(renderedCustom, "kind: StatefulSet") {
		t.Error("expected MinIO StatefulSet to be omitted when s3.minio.enabled=false")
	}
}
