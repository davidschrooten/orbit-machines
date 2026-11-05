// Package k8s implements the Kubernetes Pod manifest builder and reconciler
// for Orbit Machine Pods.
package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
)

// Kubernetes label and annotation keys used on Orbit Machine Pods.
const (
	LabelAppName      = "app.kubernetes.io/name"
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelWorkspaceID  = "orbit.app/workspace-id"
	LabelUserID       = "orbit.app/user-id"
	LabelResourceTier = "orbit.app/tier"

	AnnotationWorkspaceSlug = "orbit.app/workspace-slug"
	AnnotationUserEmail     = "orbit.app/user-email"
)

// Sentinel errors returned by PodOrchestrator implementations.
var (
	ErrClusterCapacityReached = errors.New("cluster max concurrent pods capacity reached")
	ErrPodNotFound            = errors.New("machine pod not found")
	ErrPodFailed              = errors.New("machine pod failed to start")
)

// ProgressFunc receives lifecycle progress updates while waiting for a Pod to become ready.
type ProgressFunc func(phase domain.BootPhase, message string, progressPct int)

// PodRequest describes the desired Machine Pod for a user session.
type PodRequest struct {
	MachineID               domain.MachineID
	WorkspaceSlug           string
	UserEmail               string
	Tier                    domain.ResourceTier
	ProfileEncryptionKeyHex string
	OrbitMCPToken           string
}

// PodEndpoint contains routing details for an active or provisioning Machine Pod.
type PodEndpoint struct {
	PodName      string
	PodIP        string
	WebSocketURL string
	Phase        domain.BootPhase
	Tier         domain.ResourceTier
}

// ActivePod summarizes a running or provisioning Pod for control-plane heartbeats.
type ActivePod struct {
	MachineID   domain.MachineID
	PodName     string
	Phase       domain.BootPhase
	Tier        domain.ResourceTier
	IdleSeconds int
}

// PodOrchestrator manages the lifecycle of deterministic Machine Pods in a cluster.
type PodOrchestrator interface {
	EnsurePod(ctx context.Context, req PodRequest) (PodEndpoint, error)
	WaitPodReady(ctx context.Context, mid domain.MachineID, progress ProgressFunc) (PodEndpoint, error)
	DeletePod(ctx context.Context, mid domain.MachineID) error
	ListActivePods(ctx context.Context) ([]ActivePod, error)
}

// ManifestConfig configures static cluster attributes injected into Machine Pod manifests.
type ManifestConfig struct {
	Namespace        string
	MachineImage     string
	S3Endpoint       string
	S3ProfileBucket  string
	S3NixCacheBucket string
}

// PodManifest is a lightweight JSON-serializable representation of a Kubernetes core/v1 Pod.
type PodManifest struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
	Status     PodStatus  `json:"status,omitempty"`
}

// PodList is a core/v1 PodList response.
type PodList struct {
	Items []PodManifest `json:"items"`
}

// ObjectMeta holds Kubernetes object metadata.
type ObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty"`
}

// PodSpec holds the Kubernetes Pod specification.
type PodSpec struct {
	AutomountServiceAccountToken *bool              `json:"automountServiceAccountToken"`
	SecurityContext              PodSecurityContext `json:"securityContext"`
	Volumes                      []Volume           `json:"volumes"`
	Containers                   []Container        `json:"containers"`
}

// PodSecurityContext configures Pod-level Linux security attributes.
type PodSecurityContext struct {
	RunAsNonRoot   bool           `json:"runAsNonRoot"`
	RunAsUser      int64          `json:"runAsUser"`
	RunAsGroup     int64          `json:"runAsGroup"`
	FSGroup        int64          `json:"fsGroup"`
	SeccompProfile SeccompProfile `json:"seccompProfile"`
}

// SeccompProfile configures a container or pod seccomp profile.
type SeccompProfile struct {
	Type string `json:"type"`
}

// Volume represents a Pod volume.
type Volume struct {
	Name     string          `json:"name"`
	EmptyDir *EmptyDirVolume `json:"emptyDir,omitempty"`
}

// EmptyDirVolume configures an ephemeral emptyDir volume.
type EmptyDirVolume struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// Container represents a single container inside a Pod.
type Container struct {
	Name            string                   `json:"name"`
	Image           string                   `json:"image"`
	ImagePullPolicy string                   `json:"imagePullPolicy,omitempty"`
	Args            []string                 `json:"args,omitempty"`
	Ports           []ContainerPort          `json:"ports,omitempty"`
	Env             []EnvVar                 `json:"env,omitempty"`
	Resources       ResourceRequirements     `json:"resources"`
	VolumeMounts    []VolumeMount            `json:"volumeMounts,omitempty"`
	SecurityContext ContainerSecurityContext `json:"securityContext"`
}

// ContainerPort exposes a network port from a container.
type ContainerPort struct {
	Name          string `json:"name"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol"`
}

// EnvVar is a container environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ResourceRequirements sets container CPU, memory, and storage requests and limits.
type ResourceRequirements struct {
	Requests map[string]string `json:"requests"`
	Limits   map[string]string `json:"limits"`
}

// VolumeMount mounts a Pod Volume into a container path.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

// ContainerSecurityContext configures container-level security attributes.
type ContainerSecurityContext struct {
	AllowPrivilegeEscalation bool         `json:"allowPrivilegeEscalation"`
	RunAsNonRoot             bool         `json:"runAsNonRoot"`
	Capabilities             Capabilities `json:"capabilities"`
}

// Capabilities lists Linux capabilities to add or drop.
type Capabilities struct {
	Drop []string `json:"drop"`
}

// PodStatus represents the observed runtime status of a Kubernetes Pod.
type PodStatus struct {
	Phase      string         `json:"phase,omitempty"`
	PodIP      string         `json:"podIP,omitempty"`
	Conditions []PodCondition `json:"conditions,omitempty"`
}

// PodCondition represents a condition entry in PodStatus.
type PodCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// BuildPodManifest constructs a deterministic, hardened Kubernetes v1/Pod manifest.
func BuildPodManifest(cfg ManifestConfig, req PodRequest) PodManifest {
	tier := req.Tier
	if tier == "" {
		tier = domain.TierStandard
	}
	specs := tier.Specs()
	automount := false

	namespace := strings.TrimSpace(cfg.Namespace)
	if namespace == "" {
		namespace = "orbit-machines"
	}

	env := []EnvVar{
		{Name: "ORBIT_MACHINED_LISTEN_ADDR", Value: ":8080"},
		{Name: "ORBIT_MACHINED_WORKSPACE_ID", Value: req.MachineID.WorkspaceID()},
		{Name: "ORBIT_MACHINED_USER_ID", Value: req.MachineID.UserID()},
		{Name: "ORBIT_MACHINED_RESOURCE_TIER", Value: string(tier)},
		{Name: "ORBIT_MACHINED_S3_ENDPOINT", Value: cfg.S3Endpoint},
		{Name: "ORBIT_MACHINED_S3_PROFILE_BUCKET", Value: cfg.S3ProfileBucket},
		{Name: "ORBIT_MACHINED_S3_NIX_CACHE_BUCKET", Value: cfg.S3NixCacheBucket},
	}
	if req.ProfileEncryptionKeyHex != "" {
		env = append(env, EnvVar{Name: "ORBIT_MACHINED_PROFILE_ENCRYPTION_KEY", Value: req.ProfileEncryptionKeyHex})
	}
	if req.OrbitMCPToken != "" {
		env = append(env, EnvVar{Name: "ORBIT_MCP_TOKEN", Value: req.OrbitMCPToken})
	}

	return PodManifest{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: ObjectMeta{
			Name:      req.MachineID.PodName(),
			Namespace: namespace,
			Labels: map[string]string{
				LabelAppName:      "orbit-machine",
				LabelManagedBy:    "orbit-dispatcher",
				LabelWorkspaceID:  req.MachineID.WorkspaceID(),
				LabelUserID:       req.MachineID.UserID(),
				LabelResourceTier: string(tier),
			},
			Annotations: map[string]string{
				AnnotationWorkspaceSlug: req.WorkspaceSlug,
				AnnotationUserEmail:     req.UserEmail,
			},
		},
		Spec: PodSpec{
			AutomountServiceAccountToken: &automount,
			SecurityContext: PodSecurityContext{
				RunAsNonRoot: true,
				RunAsUser:    1000,
				RunAsGroup:   1000,
				FSGroup:      1000,
				SeccompProfile: SeccompProfile{
					Type: "RuntimeDefault",
				},
			},
			Volumes: []Volume{
				{
					Name:     "nix-store",
					EmptyDir: &EmptyDirVolume{SizeLimit: specs.EphemeralStorageLimit},
				},
				{
					Name:     "workspace",
					EmptyDir: &EmptyDirVolume{SizeLimit: specs.EphemeralStorageLimit},
				},
			},
			Containers: []Container{
				{
					Name:            "machined",
					Image:           cfg.MachineImage,
					ImagePullPolicy: "IfNotPresent",
					Args:            []string{"serve"},
					Ports: []ContainerPort{
						{Name: "ws", ContainerPort: 8080, Protocol: "TCP"},
					},
					Env: env,
					Resources: ResourceRequirements{
						Requests: map[string]string{
							"cpu":    specs.CPURequest,
							"memory": specs.MemoryRequest,
						},
						Limits: map[string]string{
							"cpu":               specs.CPULimit,
							"memory":            specs.MemoryLimit,
							"ephemeral-storage": specs.EphemeralStorageLimit,
						},
					},
					VolumeMounts: []VolumeMount{
						{Name: "nix-store", MountPath: "/nix"},
						{Name: "workspace", MountPath: "/home/orbit/workspace"},
					},
					SecurityContext: ContainerSecurityContext{
						AllowPrivilegeEscalation: false,
						RunAsNonRoot:             true,
						Capabilities: Capabilities{
							Drop: []string{"ALL"},
						},
					},
				},
			},
		},
	}
}

// HTTPConfig configures the Kubernetes CoreV1 REST API orchestrator.
type HTTPConfig struct {
	APIServerURL      string
	BearerToken       string
	Manifest          ManifestConfig
	MaxConcurrentPods int
	PollInterval      time.Duration
	HTTPClient        *http.Client
}

// HTTPPodOrchestrator reconciles Machine Pods via the Kubernetes CoreV1 REST API.
type HTTPPodOrchestrator struct {
	apiServerURL      string
	bearerToken       string
	manifestCfg       ManifestConfig
	maxConcurrentPods int
	pollInterval      time.Duration
	client            *http.Client
}

// NewHTTPPodOrchestrator validates cfg and constructs an HTTPPodOrchestrator.
func NewHTTPPodOrchestrator(cfg HTTPConfig) (*HTTPPodOrchestrator, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.APIServerURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("%w: k8s apiServerURL is required", domain.ErrInvalidConfig)
	}
	if strings.TrimSpace(cfg.Manifest.Namespace) == "" {
		cfg.Manifest.Namespace = "orbit-machines"
	}
	if strings.TrimSpace(cfg.Manifest.MachineImage) == "" {
		return nil, fmt.Errorf("%w: machineImage is required", domain.ErrInvalidConfig)
	}
	if cfg.MaxConcurrentPods <= 0 {
		return nil, fmt.Errorf("%w: maxConcurrentPods must be > 0", domain.ErrInvalidConfig)
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = 250 * time.Millisecond
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPPodOrchestrator{
		apiServerURL:      baseURL,
		bearerToken:       cfg.BearerToken,
		manifestCfg:       cfg.Manifest,
		maxConcurrentPods: cfg.MaxConcurrentPods,
		pollInterval:      poll,
		client:            httpClient,
	}, nil
}

// EnsurePod returns an existing Machine Pod or creates a new one if capacity allows.
func (o *HTTPPodOrchestrator) EnsurePod(ctx context.Context, req PodRequest) (PodEndpoint, error) {
	existing, err := o.getPod(ctx, req.MachineID.PodName())
	if err == nil {
		return podEndpointFromManifest(existing), nil
	}
	if !errors.Is(err, ErrPodNotFound) {
		return PodEndpoint{}, err
	}

	active, err := o.ListActivePods(ctx)
	if err != nil {
		return PodEndpoint{}, err
	}
	if len(active) >= o.maxConcurrentPods {
		return PodEndpoint{}, fmt.Errorf("%w: %d/%d active pods", ErrClusterCapacityReached, len(active), o.maxConcurrentPods)
	}

	manifest := BuildPodManifest(o.manifestCfg, req)
	body, err := json.Marshal(manifest)
	if err != nil {
		return PodEndpoint{}, fmt.Errorf("marshaling pod manifest: %w", err)
	}

	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods", o.apiServerURL, o.manifestCfg.Namespace)
	var created PodManifest
	if err := o.doJSON(ctx, http.MethodPost, url, bytes.NewReader(body), &created); err != nil {
		return PodEndpoint{}, fmt.Errorf("creating pod %s: %w", manifest.Metadata.Name, err)
	}
	return podEndpointFromManifest(created), nil
}

// WaitPodReady polls the Pod status until the Ready condition is True and a PodIP is assigned.
func (o *HTTPPodOrchestrator) WaitPodReady(ctx context.Context, mid domain.MachineID, progress ProgressFunc) (PodEndpoint, error) {
	if progress != nil {
		progress(domain.PhaseScheduling, "Scheduling Machine Pod on Kubernetes cluster", 20)
	}

	ticker := time.NewTicker(o.pollInterval)
	defer ticker.Stop()

	notifiedHydrating := false
	for {
		pod, err := o.getPod(ctx, mid.PodName())
		if err != nil {
			return PodEndpoint{}, err
		}
		if pod.Status.Phase == "Failed" || pod.Status.Phase == "Succeeded" {
			return PodEndpoint{}, fmt.Errorf("%w: pod %s entered terminal phase %s", ErrPodFailed, mid.PodName(), pod.Status.Phase)
		}

		ep := podEndpointFromManifest(pod)
		if ep.Phase == domain.PhaseReady && ep.PodIP != "" {
			if progress != nil {
				progress(domain.PhaseReady, "Machine Pod ready", 100)
			}
			return ep, nil
		}

		if progress != nil && !notifiedHydrating && pod.Status.Phase == "Running" {
			notifiedHydrating = true
			progress(domain.PhaseHydratingProfile, "Hydrating user profile and Nix environment", 60)
		}

		select {
		case <-ctx.Done():
			return PodEndpoint{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// DeletePod deletes the Machine Pod for mid, ignoring 404 Not Found.
func (o *HTTPPodOrchestrator) DeletePod(ctx context.Context, mid domain.MachineID) error {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", o.apiServerURL, o.manifestCfg.Namespace, mid.PodName())
	err := o.doJSON(ctx, http.MethodDelete, url, nil, nil)
	if errors.Is(err, ErrPodNotFound) {
		return nil
	}
	return err
}

// ListActivePods lists all non-terminating Orbit Machine Pods in the namespace.
func (o *HTTPPodOrchestrator) ListActivePods(ctx context.Context) ([]ActivePod, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods?labelSelector=%s=orbit-machine",
		o.apiServerURL, o.manifestCfg.Namespace, LabelAppName)
	var list PodList
	if err := o.doJSON(ctx, http.MethodGet, url, nil, &list); err != nil {
		return nil, err
	}

	out := make([]ActivePod, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Metadata.DeletionTimestamp != "" {
			continue
		}
		wsID := item.Metadata.Labels[LabelWorkspaceID]
		userID := item.Metadata.Labels[LabelUserID]
		mid, err := domain.NewMachineID(wsID, userID)
		if err != nil {
			continue
		}
		tier, err := domain.ParseResourceTier(item.Metadata.Labels[LabelResourceTier])
		if err != nil {
			tier = domain.TierStandard
		}
		ep := podEndpointFromManifest(item)
		out = append(out, ActivePod{
			MachineID: mid,
			PodName:   item.Metadata.Name,
			Phase:     ep.Phase,
			Tier:      tier,
		})
	}
	return out, nil
}

func (o *HTTPPodOrchestrator) getPod(ctx context.Context, podName string) (PodManifest, error) {
	url := fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s", o.apiServerURL, o.manifestCfg.Namespace, podName)
	var pod PodManifest
	if err := o.doJSON(ctx, http.MethodGet, url, nil, &pod); err != nil {
		return PodManifest{}, err
	}
	return pod, nil
}

func (o *HTTPPodOrchestrator) doJSON(ctx context.Context, method, url string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if o.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+o.bearerToken)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrPodNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("kubernetes API %s %s returned %d: %s", method, url, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding kubernetes response: %w", err)
		}
	}
	return nil
}

func podEndpointFromManifest(pod PodManifest) PodEndpoint {
	tier, err := domain.ParseResourceTier(pod.Metadata.Labels[LabelResourceTier])
	if err != nil {
		tier = domain.TierStandard
	}

	phase := domain.PhaseScheduling
	if pod.Status.Phase == "Running" {
		phase = domain.PhaseHydratingProfile
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == "Ready" && cond.Status == "True" && pod.Status.PodIP != "" {
			phase = domain.PhaseReady
			break
		}
	}

	wsURL := ""
	if pod.Status.PodIP != "" {
		wsURL = fmt.Sprintf("ws://%s:8080/ws", pod.Status.PodIP)
	}

	return PodEndpoint{
		PodName:      pod.Metadata.Name,
		PodIP:        pod.Status.PodIP,
		WebSocketURL: wsURL,
		Phase:        phase,
		Tier:         tier,
	}
}

// FakePodOrchestrator is an in-memory PodOrchestrator for deterministic unit and integration tests.
type FakePodOrchestrator struct {
	mu                sync.Mutex
	maxConcurrentPods int
	defaultWSURL      string
	pods              map[string]ActivePod
	wsURLs            map[string]string
}

// NewFakePodOrchestrator creates an in-memory FakePodOrchestrator with the given capacity limit.
func NewFakePodOrchestrator(maxConcurrentPods int) *FakePodOrchestrator {
	if maxConcurrentPods <= 0 {
		maxConcurrentPods = 25
	}
	return &FakePodOrchestrator{
		maxConcurrentPods: maxConcurrentPods,
		defaultWSURL:      "ws://127.0.0.1:8080/ws",
		pods:              make(map[string]ActivePod),
		wsURLs:            make(map[string]string),
	}
}

// SetDefaultWebSocketURL overrides the WebSocket URL returned by WaitPodReady for newly created fake Pods.
func (f *FakePodOrchestrator) SetDefaultWebSocketURL(wsURL string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultWSURL = wsURL
}

// EnsurePod creates or reuses an in-memory fake Pod.
func (f *FakePodOrchestrator) EnsurePod(_ context.Context, req PodRequest) (PodEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	podName := req.MachineID.PodName()
	tier := req.Tier
	if tier == "" {
		tier = domain.TierStandard
	}
	if existing, ok := f.pods[podName]; ok {
		return PodEndpoint{
			PodName:      existing.PodName,
			PodIP:        "127.0.0.1",
			WebSocketURL: f.wsURLs[podName],
			Phase:        existing.Phase,
			Tier:         existing.Tier,
		}, nil
	}

	if len(f.pods) >= f.maxConcurrentPods {
		return PodEndpoint{}, fmt.Errorf("%w: %d/%d active pods", ErrClusterCapacityReached, len(f.pods), f.maxConcurrentPods)
	}

	f.pods[podName] = ActivePod{
		MachineID: req.MachineID,
		PodName:   podName,
		Phase:     domain.PhaseScheduling,
		Tier:      tier,
	}
	f.wsURLs[podName] = f.defaultWSURL

	return PodEndpoint{
		PodName:      podName,
		PodIP:        "127.0.0.1",
		WebSocketURL: f.defaultWSURL,
		Phase:        domain.PhaseScheduling,
		Tier:         tier,
	}, nil
}

// WaitPodReady transitions an in-memory fake Pod through all boot phases to PhaseReady.
func (f *FakePodOrchestrator) WaitPodReady(_ context.Context, mid domain.MachineID, progress ProgressFunc) (PodEndpoint, error) {
	f.mu.Lock()
	podName := mid.PodName()
	pod, ok := f.pods[podName]
	if !ok {
		f.mu.Unlock()
		return PodEndpoint{}, fmt.Errorf("%w: %s", ErrPodNotFound, podName)
	}
	pod.Phase = domain.PhaseReady
	f.pods[podName] = pod
	wsURL := f.wsURLs[podName]
	f.mu.Unlock()

	if progress != nil {
		progress(domain.PhaseScheduling, "Scheduling Machine Pod", 20)
		progress(domain.PhaseHydratingProfile, "Hydrating user profile from S3", 50)
		progress(domain.PhaseEvaluatingFlake, "Evaluating workspace Nix flake", 80)
		progress(domain.PhaseReady, "Machine Pod ready", 100)
	}

	return PodEndpoint{
		PodName:      podName,
		PodIP:        "127.0.0.1",
		WebSocketURL: wsURL,
		Phase:        domain.PhaseReady,
		Tier:         pod.Tier,
	}, nil
}

// DeletePod removes a fake Pod from memory.
func (f *FakePodOrchestrator) DeletePod(_ context.Context, mid domain.MachineID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pods, mid.PodName())
	delete(f.wsURLs, mid.PodName())
	return nil
}

// ListActivePods returns a snapshot of all fake Pods currently in memory.
func (f *FakePodOrchestrator) ListActivePods(_ context.Context) ([]ActivePod, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ActivePod, 0, len(f.pods))
	for _, p := range f.pods {
		out = append(out, p)
	}
	return out, nil
}
