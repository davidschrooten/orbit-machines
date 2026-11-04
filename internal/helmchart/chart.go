// Package helmchart validates the structure, default values, security invariants,
// RBAC rules, NetworkPolicy isolation, and template rendering of charts/orbit-machines.
package helmchart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ErrInvalidChart is returned when charts/orbit-machines violates a structural or security invariant.
var ErrInvalidChart = errors.New("invalid helm chart")

// RequiredTemplates lists all template files that must exist under charts/orbit-machines/templates/.
var RequiredTemplates = []string{
	"_helpers.tpl",
	"serviceaccount.yaml",
	"rbac.yaml",
	"secret.yaml",
	"deployment.yaml",
	"service.yaml",
	"ingress.yaml",
	"networkpolicy.yaml",
	"minio-init-job.yaml",
}

// RequiredDispatcherEnvVars lists the environment variables that must be wired in templates/deployment.yaml.
var RequiredDispatcherEnvVars = []string{
	"ORBIT_MACHINES_LISTEN_ADDR",
	"ORBIT_MACHINES_ORBIT_URL",
	"ORBIT_MACHINES_CLUSTER_ID",
	"ORBIT_MACHINES_CLUSTER_TOKEN",
	"ORBIT_MACHINES_SIGNING_PUBLIC_KEY",
	"ORBIT_MACHINES_SIGNING_PRIVATE_KEY",
	"ORBIT_MACHINES_NAMESPACE",
	"ORBIT_MACHINES_MACHINED_IMAGE",
	"ORBIT_MACHINES_DEFAULT_CPU",
	"ORBIT_MACHINES_DEFAULT_MEMORY",
	"ORBIT_MACHINES_WORKSPACE_STORAGE",
	"ORBIT_MACHINES_NIX_STORE_STORAGE",
	"ORBIT_MACHINES_IDLE_TIMEOUT",
	"ORBIT_MACHINES_RECONCILE_INTERVAL",
	"ORBIT_MACHINES_S3_ENDPOINT",
	"ORBIT_MACHINES_S3_REGION",
	"ORBIT_MACHINES_S3_USE_PATH_STYLE",
	"ORBIT_MACHINES_S3_ACCESS_KEY",
	"ORBIT_MACHINES_S3_SECRET_KEY",
	"ORBIT_MACHINES_S3_VAULT_BUCKET",
	"ORBIT_MACHINES_S3_GIT_WIP_BUCKET",
	"ORBIT_MACHINES_S3_NIX_CACHE_BUCKET",
	"ORBIT_DISPATCHER_LISTEN_ADDR",
	"ORBIT_DISPATCHER_ORBIT_URL",
	"ORBIT_DISPATCHER_CLUSTER_TOKEN",
	"ORBIT_DISPATCHER_KUBERNETES_NAMESPACE",
	"ORBIT_DISPATCHER_MACHINE_IMAGE",
	"ORBIT_DISPATCHER_S3_ENDPOINT",
}

// Maintainer represents a chart maintainer entry in Chart.yaml.
type Maintainer struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// ChartMetadata represents the parsed contents of Chart.yaml.
type ChartMetadata struct {
	APIVersion  string       `yaml:"apiVersion"`
	Name        string       `yaml:"name"`
	Description string       `yaml:"description"`
	Type        string       `yaml:"type"`
	Version     string       `yaml:"version"`
	AppVersion  string       `yaml:"appVersion"`
	Keywords    []string     `yaml:"keywords"`
	Maintainers []Maintainer `yaml:"maintainers"`
}

// ImageSpec configures a container image repository and tag.
type ImageSpec struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
	PullPolicy string `yaml:"pullPolicy"`
}

// DispatcherValues holds the dispatcher section of values.yaml.
type DispatcherValues struct {
	ReplicaCount      int       `yaml:"replicaCount"`
	Image             ImageSpec `yaml:"image"`
	Service           struct {
		Type string `yaml:"type"`
		Port int    `yaml:"port"`
	} `yaml:"service"`
	Ingress struct {
		Enabled     bool              `yaml:"enabled"`
		ClassName   string            `yaml:"className"`
		Host        string            `yaml:"host"`
		Annotations map[string]string `yaml:"annotations"`
		TLS         struct {
			Enabled    bool   `yaml:"enabled"`
			SecretName string `yaml:"secretName"`
		} `yaml:"tls"`
	} `yaml:"ingress"`
	Resources struct {
		Requests map[string]string `yaml:"requests"`
		Limits   map[string]string `yaml:"limits"`
	} `yaml:"resources"`
	MaxConcurrentPods int    `yaml:"maxConcurrentPods"`
	IdleTimeout       string `yaml:"idleTimeout"`
	ReconcileInterval string `yaml:"reconcileInterval"`
	HeartbeatInterval string `yaml:"heartbeatInterval"`
}

// MachinedValues holds the machined section of values.yaml.
type MachinedValues struct {
	Image            ImageSpec `yaml:"image"`
	DefaultCPU       string    `yaml:"defaultCPU"`
	DefaultMemory    string    `yaml:"defaultMemory"`
	WorkspaceStorage string    `yaml:"workspaceStorage"`
	NixStoreStorage  string    `yaml:"nixStoreStorage"`
}

// OrbitValues holds the orbit control-plane section of values.yaml.
type OrbitValues struct {
	URL               string `yaml:"url"`
	ClusterID         string `yaml:"clusterId"`
	ClusterToken      string `yaml:"clusterToken"`
	SigningPublicKey  string `yaml:"signingPublicKey"`
	SigningPrivateKey string `yaml:"signingPrivateKey"`
	ExistingSecret    string `yaml:"existingSecret"`
}

// S3Values holds the s3 and embedded minio section of values.yaml.
type S3Values struct {
	Endpoint       string `yaml:"endpoint"`
	Region         string `yaml:"region"`
	AccessKey      string `yaml:"accessKey"`
	SecretKey      string `yaml:"secretKey"`
	UsePathStyle   bool   `yaml:"usePathStyle"`
	VaultBucket    string `yaml:"vaultBucket"`
	GitWipBucket   string `yaml:"gitWipBucket"`
	NixCacheBucket string `yaml:"nixCacheBucket"`
	ProfileBucket  string `yaml:"profileBucket"`
	Minio          struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"minio"`
}

// NetworkPolicyValues holds the networkPolicy section of values.yaml.
type NetworkPolicyValues struct {
	Enabled               bool `yaml:"enabled"`
	AllowClusterAPIServer bool `yaml:"allowClusterAPIServer"`
}

// Values represents the parsed contents of values.yaml.
type Values struct {
	Dispatcher    DispatcherValues    `yaml:"dispatcher"`
	Machined      MachinedValues      `yaml:"machined"`
	Orbit         OrbitValues         `yaml:"orbit"`
	S3            S3Values            `yaml:"s3"`
	NetworkPolicy NetworkPolicyValues `yaml:"networkPolicy"`
}

// InspectionReport contains the parsed metadata, default values, and raw templates of the chart.
type InspectionReport struct {
	Metadata  ChartMetadata
	Values    Values
	Templates map[string]string
}

// InspectChart loads Chart.yaml, values.yaml, and all required templates from chartDir.
func InspectChart(chartDir string) (InspectionReport, error) {
	chartBytes, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		return InspectionReport{}, fmt.Errorf("%w: reading Chart.yaml: %w", ErrInvalidChart, err)
	}
	var meta ChartMetadata
	if err := yaml.Unmarshal(chartBytes, &meta); err != nil {
		return InspectionReport{}, fmt.Errorf("%w: parsing Chart.yaml: %w", ErrInvalidChart, err)
	}

	valuesBytes, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		return InspectionReport{}, fmt.Errorf("%w: reading values.yaml: %w", ErrInvalidChart, err)
	}
	var vals Values
	if err := yaml.Unmarshal(valuesBytes, &vals); err != nil {
		return InspectionReport{}, fmt.Errorf("%w: parsing values.yaml: %w", ErrInvalidChart, err)
	}

	templates := make(map[string]string, len(RequiredTemplates))
	for _, name := range RequiredTemplates {
		raw, err := os.ReadFile(filepath.Join(chartDir, "templates", name))
		if err != nil {
			return InspectionReport{}, fmt.Errorf("%w: missing required template %s: %w", ErrInvalidChart, name, err)
		}
		templates[name] = string(raw)
	}

	return InspectionReport{
		Metadata:  meta,
		Values:    vals,
		Templates: templates,
	}, nil
}

// ValidateChart checks all metadata, default values, security, RBAC, and NetworkPolicy invariants.
func ValidateChart(chartDir string) error {
	report, err := InspectChart(chartDir)
	if err != nil {
		return err
	}
	if err := validateMetadata(report.Metadata); err != nil {
		return err
	}
	if err := validateValues(report.Values); err != nil {
		return err
	}
	return validateTemplates(report.Templates)
}

func validateMetadata(meta ChartMetadata) error {
	if meta.APIVersion != "v2" {
		return fmt.Errorf("%w: Chart.yaml apiVersion must be v2, got %q", ErrInvalidChart, meta.APIVersion)
	}
	if meta.Name != "orbit-machines" {
		return fmt.Errorf("%w: Chart.yaml name must be orbit-machines, got %q", ErrInvalidChart, meta.Name)
	}
	if strings.TrimSpace(meta.Version) == "" || strings.TrimSpace(meta.AppVersion) == "" {
		return fmt.Errorf("%w: Chart.yaml version and appVersion are required", ErrInvalidChart)
	}
	if strings.TrimSpace(meta.Description) == "" || len(meta.Keywords) == 0 || len(meta.Maintainers) == 0 {
		return fmt.Errorf("%w: Chart.yaml description, keywords, and maintainers are required", ErrInvalidChart)
	}
	return nil
}

func validateValues(v Values) error {
	if v.Dispatcher.ReplicaCount <= 0 {
		return fmt.Errorf("%w: dispatcher.replicaCount must be > 0", ErrInvalidChart)
	}
	if v.Dispatcher.Image.Repository != "ghcr.io/davidschrooten/orbit-dispatcher" || strings.TrimSpace(v.Dispatcher.Image.Tag) == "" {
		return fmt.Errorf("%w: dispatcher.image must use ghcr.io/davidschrooten/orbit-dispatcher with a non-empty tag", ErrInvalidChart)
	}
	if v.Dispatcher.Service.Port != 8080 {
		return fmt.Errorf("%w: dispatcher.service.port must default to 8080, got %d", ErrInvalidChart, v.Dispatcher.Service.Port)
	}
	if strings.TrimSpace(v.Dispatcher.IdleTimeout) == "" || strings.TrimSpace(v.Dispatcher.ReconcileInterval) == "" {
		return fmt.Errorf("%w: dispatcher.idleTimeout and dispatcher.reconcileInterval are required", ErrInvalidChart)
	}
	if v.Machined.Image.Repository != "ghcr.io/davidschrooten/orbit-machined" || strings.TrimSpace(v.Machined.Image.Tag) == "" {
		return fmt.Errorf("%w: machined.image must use ghcr.io/davidschrooten/orbit-machined with a non-empty tag", ErrInvalidChart)
	}
	if v.Machined.DefaultCPU == "" || v.Machined.DefaultMemory == "" || v.Machined.WorkspaceStorage == "" || v.Machined.NixStoreStorage == "" {
		return fmt.Errorf("%w: machined defaultCPU, defaultMemory, workspaceStorage, and nixStoreStorage are required", ErrInvalidChart)
	}
	if strings.TrimSpace(v.Orbit.URL) == "" || strings.TrimSpace(v.Orbit.ClusterID) == "" || strings.TrimSpace(v.Orbit.ClusterToken) == "" {
		return fmt.Errorf("%w: orbit.url, orbit.clusterId, and orbit.clusterToken defaults are required", ErrInvalidChart)
	}
	if !v.S3.UsePathStyle || v.S3.VaultBucket != "orbit-vaults" || v.S3.GitWipBucket != "orbit-git-wip" || v.S3.NixCacheBucket != "orbit-nix-cache" || !v.S3.Minio.Enabled {
		return fmt.Errorf("%w: s3 defaults must enable usePathStyle, minio, and standard bucket names", ErrInvalidChart)
	}
	if !v.NetworkPolicy.Enabled || !v.NetworkPolicy.AllowClusterAPIServer {
		return fmt.Errorf("%w: networkPolicy.enabled and networkPolicy.allowClusterAPIServer must default to true", ErrInvalidChart)
	}
	return nil
}

func validateTemplates(tpls map[string]string) error {
	deploy := tpls["deployment.yaml"]
	for _, probe := range []string{"/healthz", "/readyz", "runAsNonRoot: true", "allowPrivilegeEscalation: false"} {
		if !strings.Contains(deploy, probe) {
			return fmt.Errorf("%w: deployment.yaml missing %q", ErrInvalidChart, probe)
		}
	}
	for _, envName := range RequiredDispatcherEnvVars {
		if !strings.Contains(deploy, envName) {
			return fmt.Errorf("%w: deployment.yaml missing env var %s", ErrInvalidChart, envName)
		}
	}

	rbac := tpls["rbac.yaml"]
	if strings.Contains(rbac, "ClusterRole") {
		return fmt.Errorf("%w: rbac.yaml must be namespace-scoped Role/RoleBinding, not ClusterRole", ErrInvalidChart)
	}
	for _, res := range []string{`"pods"`, `"pods/log"`, `"persistentvolumeclaims"`, `"secrets"`} {
		if !strings.Contains(rbac, res) {
			return fmt.Errorf("%w: rbac.yaml missing resource %s", ErrInvalidChart, res)
		}
	}

	netpol := tpls["networkpolicy.yaml"]
	for _, token := range []string{
		"app.kubernetes.io/component: machine-pod",
		"app.kubernetes.io/component: dispatcher",
		"port: 8080",
		"port: 53",
		"port: 9000",
		"port: 443",
		"port: 22",
		"169.254.169.254/32",
	} {
		if !strings.Contains(netpol, token) {
			return fmt.Errorf("%w: networkpolicy.yaml missing %q", ErrInvalidChart, token)
		}
	}

	minioJob := tpls["minio-init-job.yaml"]
	for _, token := range []string{"vaultBucket", "gitWipBucket", "nixCacheBucket", "post-install,post-upgrade"} {
		if !strings.Contains(minioJob, token) {
			return fmt.Errorf("%w: minio-init-job.yaml missing %q", ErrInvalidChart, token)
		}
	}

	return nil
}

// LintWithHelm executes `helm lint <chartDir>` using the given helm binary.
func LintWithHelm(ctx context.Context, helmBin, chartDir string) (string, error) {
	if strings.TrimSpace(helmBin) == "" {
		helmBin = "helm"
	}
	cmd := exec.CommandContext(ctx, helmBin, "lint", chartDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("helm lint failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RenderWithHelm executes `helm template orbit-machines <chartDir>` with optional --set overrides.
func RenderWithHelm(ctx context.Context, helmBin, chartDir string, setValues map[string]string) (string, error) {
	if strings.TrimSpace(helmBin) == "" {
		helmBin = "helm"
	}
	args := []string{"template", "orbit-machines", chartDir, "--namespace", "orbit-machines"}
	keys := make([]string, 0, len(setValues))
	for k := range setValues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--set", fmt.Sprintf("%s=%s", k, setValues[k]))
	}

	cmd := exec.CommandContext(ctx, helmBin, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("helm template failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
