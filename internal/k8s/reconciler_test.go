package k8s_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/k8s"
)

func TestBuildPodManifest_SecurityAndTierSpecs(t *testing.T) {
	t.Parallel()

	mid, err := domain.NewMachineID("ws_01HXYZ", "usr_01HABC")
	if err != nil {
		t.Fatalf("NewMachineID: %v", err)
	}

	cfg := k8s.ManifestConfig{
		Namespace:        "orbit-machines",
		MachineImage:     "ghcr.io/davidschrooten/orbit-machined:0.1.0",
		S3Endpoint:       "http://orbit-machines-minio:9000",
		S3ProfileBucket:  "orbit-user-profiles",
		S3NixCacheBucket: "orbit-nix-cache",
	}

	for _, tc := range []struct {
		name      string
		tier      domain.ResourceTier
		wantCPU   string
		wantMem   string
		wantStore string
	}{
		{
			name:      "standard tier",
			tier:      domain.TierStandard,
			wantCPU:   "2000m",
			wantMem:   "4Gi",
			wantStore: "20Gi",
		},
		{
			name:      "performance tier",
			tier:      domain.TierPerformance,
			wantCPU:   "4000m",
			wantMem:   "8Gi",
			wantStore: "40Gi",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := k8s.PodRequest{
				MachineID:               mid,
				WorkspaceSlug:           "acme",
				UserEmail:               "alice@example.com",
				Tier:                    tc.tier,
				ProfileEncryptionKeyHex: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				OrbitURL:                "https://orbit.acme.dev",
				OrbitMCPToken:           "omcp_tok_123",
				RepositoriesJSON:        `[{"name":"orbit","clone_url":"https://github.com/acme/orbit.git"}]`,
			}

			manifest := k8s.BuildPodManifest(cfg, req)
			if manifest.APIVersion != "v1" || manifest.Kind != "Pod" {
				t.Fatalf("unexpected apiVersion/kind: %s/%s", manifest.APIVersion, manifest.Kind)
			}
			if manifest.Metadata.Name != mid.PodName() {
				t.Fatalf("Name = %q, want %q", manifest.Metadata.Name, mid.PodName())
			}
			if manifest.Metadata.Namespace != "orbit-machines" {
				t.Fatalf("Namespace = %q, want orbit-machines", manifest.Metadata.Namespace)
			}
			if manifest.Metadata.Labels[k8s.LabelWorkspaceID] != "ws_01HXYZ" {
				t.Fatalf("workspace label = %q, want ws_01HXYZ", manifest.Metadata.Labels[k8s.LabelWorkspaceID])
			}
			if manifest.Metadata.Labels[k8s.LabelUserID] != "usr_01HABC" {
				t.Fatalf("user label = %q, want usr_01HABC", manifest.Metadata.Labels[k8s.LabelUserID])
			}
			if manifest.Metadata.Labels[k8s.LabelResourceTier] != string(tc.tier) {
				t.Fatalf("tier label = %q, want %q", manifest.Metadata.Labels[k8s.LabelResourceTier], tc.tier)
			}
			if manifest.Spec.AutomountServiceAccountToken == nil || *manifest.Spec.AutomountServiceAccountToken {
				t.Fatalf("expected automountServiceAccountToken = false")
			}
			if !manifest.Spec.SecurityContext.RunAsNonRoot || manifest.Spec.SecurityContext.RunAsUser != 1000 || manifest.Spec.SecurityContext.FSGroup != 1000 {
				t.Fatalf("unexpected pod security context: %+v", manifest.Spec.SecurityContext)
			}
			if manifest.Spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
				t.Fatalf("unexpected seccomp profile: %+v", manifest.Spec.SecurityContext.SeccompProfile)
			}
			if len(manifest.Spec.Containers) != 1 {
				t.Fatalf("expected 1 container, got %d", len(manifest.Spec.Containers))
			}

			c := manifest.Spec.Containers[0]
			if c.Image != cfg.MachineImage {
				t.Fatalf("container image = %q, want %q", c.Image, cfg.MachineImage)
			}
			if c.Resources.Limits["cpu"] != tc.wantCPU {
				t.Fatalf("cpu limit = %q, want %q", c.Resources.Limits["cpu"], tc.wantCPU)
			}
			if c.Resources.Limits["memory"] != tc.wantMem {
				t.Fatalf("memory limit = %q, want %q", c.Resources.Limits["memory"], tc.wantMem)
			}
			if c.Resources.Limits["ephemeral-storage"] != tc.wantStore {
				t.Fatalf("ephemeral-storage limit = %q, want %q", c.Resources.Limits["ephemeral-storage"], tc.wantStore)
			}
			if c.SecurityContext.AllowPrivilegeEscalation || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
				t.Fatalf("unexpected container security context: %+v", c.SecurityContext)
			}

			envMap := make(map[string]string, len(c.Env))
			for _, e := range c.Env {
				envMap[e.Name] = e.Value
			}
			for k, wantVal := range map[string]string{
				"ORBIT_URL":               "https://orbit.acme.dev",
				"ORBIT_MCP_TOKEN":         "omcp_tok_123",
				"ORBIT_WORKSPACE_SLUG":    "acme",
				"ORBIT_USER_EMAIL":        "alice@example.com",
				"ORBIT_REPOSITORIES_JSON": `[{"name":"orbit","clone_url":"https://github.com/acme/orbit.git"}]`,
			} {
				if envMap[k] != wantVal {
					t.Fatalf("container env %s = %q, want %q", k, envMap[k], wantVal)
				}
			}
		})
	}
}

func TestHTTPPodOrchestrator_EnsureWaitListDeleteLifecycle(t *testing.T) {
	t.Parallel()

	var (
		mu       sync.Mutex
		pods     = make(map[string]k8s.PodManifest)
		getCount atomic.Int32
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k8s-sa-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/orbit-machines/pods":
			items := make([]k8s.PodManifest, 0, len(pods))
			for _, p := range pods {
				items = append(items, p)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(k8s.PodList{Items: items})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/orbit-machines/pods":
			var manifest k8s.PodManifest
			if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			manifest.Status = k8s.PodStatus{Phase: "Pending"}
			pods[manifest.Metadata.Name] = manifest
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(manifest)

		case r.Method == http.MethodGet && len(r.URL.Path) > len("/api/v1/namespaces/orbit-machines/pods/"):
			name := r.URL.Path[len("/api/v1/namespaces/orbit-machines/pods/"):]
			p, ok := pods[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			calls := getCount.Add(1)
			if calls >= 2 {
				p.Status = k8s.PodStatus{
					Phase: "Running",
					PodIP: "10.244.0.42",
					Conditions: []k8s.PodCondition{
						{Type: "Ready", Status: "True"},
					},
				}
				pods[name] = p
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p)

		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/api/v1/namespaces/orbit-machines/pods/"):
			name := r.URL.Path[len("/api/v1/namespaces/orbit-machines/pods/"):]
			if _, ok := pods[name]; !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			delete(pods, name)
			w.WriteHeader(http.StatusOK)

		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	orch, err := k8s.NewHTTPPodOrchestrator(k8s.HTTPConfig{
		APIServerURL: srv.URL,
		BearerToken:  "k8s-sa-token",
		Manifest: k8s.ManifestConfig{
			Namespace:        "orbit-machines",
			MachineImage:     "ghcr.io/davidschrooten/orbit-machined:0.1.0",
			S3Endpoint:       "http://orbit-machines-minio:9000",
			S3ProfileBucket:  "orbit-user-profiles",
			S3NixCacheBucket: "orbit-nix-cache",
		},
		MaxConcurrentPods: 2,
		PollInterval:      10 * time.Millisecond,
		HTTPClient:        srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewHTTPPodOrchestrator: %v", err)
	}

	mid, _ := domain.NewMachineID("ws_01", "usr_01")
	req := k8s.PodRequest{
		MachineID:     mid,
		WorkspaceSlug: "acme",
		UserEmail:     "alice@example.com",
		Tier:          domain.TierStandard,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First EnsurePod creates the Pod.
	ep, err := orch.EnsurePod(ctx, req)
	if err != nil {
		t.Fatalf("EnsurePod create: %v", err)
	}
	if ep.PodName != mid.PodName() {
		t.Fatalf("PodName = %q, want %q", ep.PodName, mid.PodName())
	}

	// WaitPodReady polls until Ready and emits progress callbacks.
	var phases []domain.BootPhase
	readyEP, err := orch.WaitPodReady(ctx, mid, func(phase domain.BootPhase, _ string, _ int) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("WaitPodReady: %v", err)
	}
	if readyEP.PodIP != "10.244.0.42" || readyEP.WebSocketURL != "ws://10.244.0.42:8080/ws" {
		t.Fatalf("unexpected ready endpoint: %+v", readyEP)
	}
	if len(phases) == 0 || phases[len(phases)-1] != domain.PhaseReady {
		t.Fatalf("expected final progress phase Ready, got %v", phases)
	}

	// Second EnsurePod reuses existing Ready Pod.
	reusedEP, err := orch.EnsurePod(ctx, req)
	if err != nil {
		t.Fatalf("EnsurePod reuse: %v", err)
	}
	if reusedEP.PodIP != "10.244.0.42" || reusedEP.Phase != domain.PhaseReady {
		t.Fatalf("unexpected reused endpoint: %+v", reusedEP)
	}

	// ListActivePods returns the active Pod.
	active, err := orch.ListActivePods(ctx)
	if err != nil {
		t.Fatalf("ListActivePods: %v", err)
	}
	if len(active) != 1 || active[0].MachineID != mid || active[0].Tier != domain.TierStandard {
		t.Fatalf("unexpected active pods: %+v", active)
	}

	// DeletePod deletes it; second DeletePod is idempotent on 404.
	if err := orch.DeletePod(ctx, mid); err != nil {
		t.Fatalf("DeletePod first: %v", err)
	}
	if err := orch.DeletePod(ctx, mid); err != nil {
		t.Fatalf("DeletePod idempotent 404: %v", err)
	}
}

func TestHTTPPodOrchestrator_CapacityLimitAndValidation(t *testing.T) {
	t.Parallel()

	if _, err := k8s.NewHTTPPodOrchestrator(k8s.HTTPConfig{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty HTTPConfig, got %v", err)
	}

	fake := k8s.NewFakePodOrchestrator(1)
	ctx := context.Background()

	mid1, _ := domain.NewMachineID("ws_01", "usr_01")
	mid2, _ := domain.NewMachineID("ws_01", "usr_02")

	if _, err := fake.EnsurePod(ctx, k8s.PodRequest{MachineID: mid1, Tier: domain.TierStandard}); err != nil {
		t.Fatalf("EnsurePod 1: %v", err)
	}
	// Reusing mid1 within limit succeeds.
	if _, err := fake.EnsurePod(ctx, k8s.PodRequest{MachineID: mid1, Tier: domain.TierStandard}); err != nil {
		t.Fatalf("EnsurePod 1 reuse: %v", err)
	}
	// Creating mid2 exceeds capacity 1.
	if _, err := fake.EnsurePod(ctx, k8s.PodRequest{MachineID: mid2, Tier: domain.TierStandard}); !errors.Is(err, k8s.ErrClusterCapacityReached) {
		t.Fatalf("expected ErrClusterCapacityReached, got %v", err)
	}

	var seenPhases []domain.BootPhase
	ep, err := fake.WaitPodReady(ctx, mid1, func(phase domain.BootPhase, _ string, _ int) {
		seenPhases = append(seenPhases, phase)
	})
	if err != nil {
		t.Fatalf("Fake WaitPodReady: %v", err)
	}
	if ep.Phase != domain.PhaseReady {
		t.Fatalf("expected PhaseReady, got %q", ep.Phase)
	}
	if len(seenPhases) != 4 {
		t.Fatalf("expected 4 boot phases from FakePodOrchestrator, got %v", seenPhases)
	}

	active, err := fake.ListActivePods(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("ListActivePods = %v, %v", active, err)
	}
	if err := fake.DeletePod(ctx, mid1); err != nil {
		t.Fatalf("DeletePod: %v", err)
	}
	if _, err := fake.WaitPodReady(ctx, mid1, nil); !errors.Is(err, k8s.ErrPodNotFound) {
		t.Fatalf("expected ErrPodNotFound after delete, got %v", err)
	}
}

func TestHTTPPodOrchestrator_EdgeCasesAndErrors(t *testing.T) {
	t.Parallel()

	// Validate constructor error branches.
	for _, tc := range []struct {
		name string
		cfg  k8s.HTTPConfig
	}{
		{
			name: "missing machine image",
			cfg:  k8s.HTTPConfig{APIServerURL: "https://k8s.local", MaxConcurrentPods: 5},
		},
		{
			name: "zero max concurrent pods",
			cfg: k8s.HTTPConfig{
				APIServerURL: "https://k8s.local",
				Manifest:     k8s.ManifestConfig{MachineImage: "img:1"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := k8s.NewHTTPPodOrchestrator(tc.cfg); !errors.Is(err, domain.ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig, got %v", err)
			}
		})
	}

	// BuildPodManifest defaults when namespace and tier are empty.
	mid, _ := domain.NewMachineID("ws_def", "usr_def")
	defManifest := k8s.BuildPodManifest(k8s.ManifestConfig{MachineImage: "img:1"}, k8s.PodRequest{MachineID: mid})
	if defManifest.Metadata.Namespace != "orbit-machines" || defManifest.Metadata.Labels[k8s.LabelResourceTier] != string(domain.TierStandard) {
		t.Fatalf("unexpected default manifest metadata: %+v", defManifest.Metadata)
	}

	// FakePodOrchestrator default capacity & SetDefaultWebSocketURL.
	fake := k8s.NewFakePodOrchestrator(0)
	fake.SetDefaultWebSocketURL("ws://10.0.0.9:8080/ws")
	ep, err := fake.EnsurePod(context.Background(), k8s.PodRequest{MachineID: mid})
	if err != nil || ep.WebSocketURL != "ws://10.0.0.9:8080/ws" || ep.Tier != domain.TierStandard {
		t.Fatalf("unexpected fake EnsurePod: %+v, err=%v", ep, err)
	}

	// Server simulating capacity limit, Failed pod phase, terminating pods, and 500 error.
	var mode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1: // Capacity reached on list
			if r.URL.Path == "/api/v1/namespaces/orbit-machines/pods" && r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(k8s.PodList{
					Items: []k8s.PodManifest{
						{
							Metadata: k8s.ObjectMeta{
								Name:   "machine-ws-other-usr-other",
								Labels: map[string]string{k8s.LabelWorkspaceID: "ws_other", k8s.LabelUserID: "usr_other", k8s.LabelResourceTier: "invalid"},
							},
						},
						{
							Metadata: k8s.ObjectMeta{
								Name:              "terminating-pod",
								DeletionTimestamp: "2026-11-04T00:00:00Z",
							},
						},
						{
							Metadata: k8s.ObjectMeta{
								Name:   "missing-labels",
								Labels: map[string]string{},
							},
						},
					},
				})
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
		case 2: // Failed Pod phase during WaitPodReady
			_ = json.NewEncoder(w).Encode(k8s.PodManifest{
				Metadata: k8s.ObjectMeta{Name: mid.PodName()},
				Status:   k8s.PodStatus{Phase: "Failed"},
			})
		case 3: // Pending forever for context cancellation test
			_ = json.NewEncoder(w).Encode(k8s.PodManifest{
				Metadata: k8s.ObjectMeta{Name: mid.PodName()},
				Status:   k8s.PodStatus{Phase: "Running"},
			})
		default: // 500 internal server error
			http.Error(w, "apiserver boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	orch, err := k8s.NewHTTPPodOrchestrator(k8s.HTTPConfig{
		APIServerURL:      srv.URL,
		Manifest:          k8s.ManifestConfig{MachineImage: "img:1"},
		MaxConcurrentPods: 1,
	})
	if err != nil {
		t.Fatalf("NewHTTPPodOrchestrator: %v", err)
	}

	// Mode 0: 500 error
	if _, err := orch.EnsurePod(context.Background(), k8s.PodRequest{MachineID: mid}); err == nil {
		t.Fatalf("expected error on 500 response")
	}

	// Mode 1: Capacity reached
	mode.Store(1)
	if _, err := orch.EnsurePod(context.Background(), k8s.PodRequest{MachineID: mid}); !errors.Is(err, k8s.ErrClusterCapacityReached) {
		t.Fatalf("expected ErrClusterCapacityReached, got %v", err)
	}

	// Mode 2: Failed Pod phase
	mode.Store(2)
	if _, err := orch.WaitPodReady(context.Background(), mid, nil); !errors.Is(err, k8s.ErrPodFailed) {
		t.Fatalf("expected ErrPodFailed, got %v", err)
	}

	// Mode 3: Context canceled during WaitPodReady
	mode.Store(3)
	shortCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := orch.WaitPodReady(shortCtx, mid, func(domain.BootPhase, string, int) {}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestHTTPPodOrchestrator_EnsureWorkspacePVCLifecycle(t *testing.T) {
	t.Parallel()

	var (
		mu   sync.Mutex
		pvcs = make(map[string]k8s.PVCManifest)
		pods = make(map[string]k8s.PodManifest)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/orbit-machines/pods":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(k8s.PodList{})

		case r.Method == http.MethodGet && len(r.URL.Path) > len("/api/v1/namespaces/orbit-machines/pods/"):
			name := r.URL.Path[len("/api/v1/namespaces/orbit-machines/pods/"):]
			p, ok := pods[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(p)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/orbit-machines/pods":
			var manifest k8s.PodManifest
			_ = json.NewDecoder(r.Body).Decode(&manifest)
			pods[manifest.Metadata.Name] = manifest
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(manifest)

		case r.Method == http.MethodDelete && len(r.URL.Path) > len("/api/v1/namespaces/orbit-machines/pods/"):
			name := r.URL.Path[len("/api/v1/namespaces/orbit-machines/pods/"):]
			delete(pods, name)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && len(r.URL.Path) > len("/api/v1/namespaces/orbit-machines/persistentvolumeclaims/"):
			name := r.URL.Path[len("/api/v1/namespaces/orbit-machines/persistentvolumeclaims/"):]
			pvc, ok := pvcs[name]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(pvc)

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/orbit-machines/persistentvolumeclaims":
			var pvc k8s.PVCManifest
			_ = json.NewDecoder(r.Body).Decode(&pvc)
			pvcs[pvc.Metadata.Name] = pvc
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pvc)

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	orch, err := k8s.NewHTTPPodOrchestrator(k8s.HTTPConfig{
		APIServerURL:      srv.URL,
		Manifest:          k8s.ManifestConfig{Namespace: "orbit-machines", MachineImage: "img:1"},
		MaxConcurrentPods: 5,
		HTTPClient:        srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewHTTPPodOrchestrator: %v", err)
	}

	mid, _ := domain.NewMachineID("ws_pvc", "usr_pvc")
	ctx := context.Background()

	// 1. First EnsurePod creates the PVC and mounts it on the Pod's workspace volume.
	if _, err := orch.EnsurePod(ctx, k8s.PodRequest{MachineID: mid}); err != nil {
		t.Fatalf("EnsurePod 1: %v", err)
	}
	wantPVC := mid.PodName() + "-ws"
	mu.Lock()
	createdPod := pods[mid.PodName()]
	_, pvcExists := pvcs[wantPVC]
	mu.Unlock()
	if !pvcExists {
		t.Fatalf("expected PVC %s to be created", wantPVC)
	}
	var wsVol *k8s.Volume
	for i := range createdPod.Spec.Volumes {
		if createdPod.Spec.Volumes[i].Name == "workspace" {
			wsVol = &createdPod.Spec.Volumes[i]
		}
	}
	if wsVol == nil || wsVol.PersistentVolumeClaim == nil || wsVol.PersistentVolumeClaim.ClaimName != wantPVC {
		t.Fatalf("expected workspace volume to mount PVC %q, got %+v", wantPVC, wsVol)
	}

	// 2. Delete Pod and call EnsurePod again -> reuses existing PVC.
	_ = orch.DeletePod(ctx, mid)
	if _, err := orch.EnsurePod(ctx, k8s.PodRequest{MachineID: mid}); err != nil {
		t.Fatalf("EnsurePod 2 (reuse PVC): %v", err)
	}
}
