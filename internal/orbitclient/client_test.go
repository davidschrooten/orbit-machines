package orbitclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/orbitclient"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
)

const testClusterToken = "omc_live_9876543210abcdef9876543210abcdef"

func TestClient_RegisterHeartbeatAndMintCredentials(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testClusterToken {
			http.Error(w, `{"error":"invalid cluster token"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/machine_clusters/register":
			var req protocol.RegisterClusterRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.RegisterClusterResponse{
				ClusterID:                "mc_01HXYZ",
				WorkspaceID:              "ws_01HXYZ",
				WorkspaceSlug:            "acme",
				Status:                   "active",
				HeartbeatIntervalSeconds: 30,
			})

		case "/api/v1/machine_clusters/heartbeat":
			var req protocol.HeartbeatRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{
				Status:         "ok",
				RevokeSessions: []string{"usr_revoked"},
			})

		case "/api/v1/machine_clusters/credentials":
			var req protocol.MintCredentialsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.MintCredentialsResponse{
				OrbitMCPToken: "omcp_ephemeral_token",
				Repositories: []protocol.RepoCredential{
					{
						ID:            "repo_01",
						Name:          "orbit",
						CloneURL:      "https://github.com/acme/orbit.git",
						DefaultBranch: "main",
						GitUsername:   "x-access-token",
						GitToken:      "ghs_ephemeral",
						ExpiresAt:     time.Date(2026, 11, 4, 16, 0, 0, 0, time.UTC),
					},
				},
			})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := orbitclient.NewClient(srv.URL, testClusterToken, orbitclient.WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()

	regResp, err := client.RegisterCluster(ctx, protocol.RegisterClusterRequest{
		ClusterVersion:      "0.1.0",
		DispatcherURL:       "https://machines.acme.dev",
		IngressHost:         "machines.acme.dev",
		KubernetesNamespace: "orbit-machines",
		MinIOEnabled:        true,
		MaxConcurrentPods:   25,
	})
	if err != nil {
		t.Fatalf("RegisterCluster: %v", err)
	}
	if regResp.ClusterID != "mc_01HXYZ" || regResp.WorkspaceSlug != "acme" {
		t.Fatalf("unexpected RegisterClusterResponse: %+v", regResp)
	}

	hbResp, err := client.SendHeartbeat(ctx, protocol.HeartbeatRequest{
		ActivePods:        1,
		MaxConcurrentPods: 25,
		Sessions: []protocol.SessionStatus{
			{
				WorkspaceID: "ws_01HXYZ",
				Subject:     "usr_01",
				PodName:     "machine-ws-01hxyz-usr-01",
				Phase:       domain.PhaseReady,
				Tier:        domain.TierStandard,
			},
		},
	})
	if err != nil {
		t.Fatalf("SendHeartbeat: %v", err)
	}
	if len(hbResp.RevokeSessions) != 1 || hbResp.RevokeSessions[0] != "usr_revoked" {
		t.Fatalf("unexpected HeartbeatResponse: %+v", hbResp)
	}

	credResp, err := client.MintCredentials(ctx, protocol.MintCredentialsRequest{
		WorkspaceID: "ws_01HXYZ",
		Subject:     "usr_01",
		RepoIDs:     []string{"repo_01"},
	})
	if err != nil {
		t.Fatalf("MintCredentials: %v", err)
	}
	if credResp.OrbitMCPToken != "omcp_ephemeral_token" || len(credResp.Repositories) != 1 {
		t.Fatalf("unexpected MintCredentialsResponse: %+v", credResp)
	}
}

func TestClient_ValidationAndAuthErrors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		url   string
		token string
	}{
		{name: "empty url", url: "", token: testClusterToken},
		{name: "invalid url scheme", url: "ftp://orbit.local", token: testClusterToken},
		{name: "short token", url: "https://orbit.local", token: "short"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := orbitclient.NewClient(tc.url, tc.token); !errors.Is(err, domain.ErrInvalidConfig) {
				t.Fatalf("expected ErrInvalidConfig, got %v", err)
			}
		})
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/machine_clusters/register" {
			http.Error(w, "unauthorized cluster", http.StatusUnauthorized)
			return
		}
		http.Error(w, "server error", http.StatusBadGateway)
	}))
	defer srv.Close()

	client, err := orbitclient.NewClient(srv.URL, testClusterToken)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	if _, err := client.RegisterCluster(context.Background(), protocol.RegisterClusterRequest{}); !errors.Is(err, orbitclient.ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if _, err := client.SendHeartbeat(context.Background(), protocol.HeartbeatRequest{}); err == nil {
		t.Fatalf("expected error on 502 status")
	}
	if _, err := client.MintCredentials(context.Background(), protocol.MintCredentialsRequest{}); err == nil {
		t.Fatalf("expected error on 502 status for MintCredentials")
	}

	badJSONSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not-json"))
	}))
	defer badJSONSrv.Close()
	badClient, _ := orbitclient.NewClient(badJSONSrv.URL, testClusterToken)
	if _, err := badClient.RegisterCluster(context.Background(), protocol.RegisterClusterRequest{}); err == nil {
		t.Fatalf("expected decode error on corrupt JSON")
	}

	// Verify FakeClient round-trip.
	fake := orbitclient.NewFakeClient()
	fake.RevokeOnHeartbeat = []string{"usr_gone"}
	hb, err := fake.SendHeartbeat(context.Background(), protocol.HeartbeatRequest{ActivePods: 2})
	if err != nil || len(hb.RevokeSessions) != 1 || hb.RevokeSessions[0] != "usr_gone" {
		t.Fatalf("unexpected fake heartbeat: %+v, err=%v", hb, err)
	}
	reg, err := fake.RegisterCluster(context.Background(), protocol.RegisterClusterRequest{ClusterVersion: "0.1.0"})
	if err != nil || reg.Status != "active" {
		t.Fatalf("unexpected fake register: %+v, err=%v", reg, err)
	}
	creds, err := fake.MintCredentials(context.Background(), protocol.MintCredentialsRequest{WorkspaceID: "ws_1", Subject: "usr_1"})
	if err != nil || creds.OrbitMCPToken == "" {
		t.Fatalf("unexpected fake credentials: %+v, err=%v", creds, err)
	}
}
