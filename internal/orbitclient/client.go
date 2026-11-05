// Package orbitclient provides the REST control-plane client used by
// orbit-dispatcher to register with the Orbit Rails monolith, send cluster
// heartbeats, and mint short-lived session credentials.
package orbitclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/ticket"
)

// ErrUnauthorized is returned when the Orbit control plane rejects the cluster token.
var ErrUnauthorized = errors.New("orbit control plane rejected cluster token")

// ControlPlaneClient defines the REST contract between orbit-dispatcher and Orbit.
type ControlPlaneClient interface {
	RegisterCluster(ctx context.Context, req protocol.RegisterClusterRequest) (protocol.RegisterClusterResponse, error)
	SendHeartbeat(ctx context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error)
	MintCredentials(ctx context.Context, req protocol.MintCredentialsRequest) (protocol.MintCredentialsResponse, error)
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithHTTPClient overrides the default *http.Client used by Client.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

// Client implements ControlPlaneClient over HTTP/HTTPS.
type Client struct {
	baseURL      string
	clusterToken string
	httpClient   *http.Client
}

// NewClient validates baseURL and clusterToken and returns a configured Client.
func NewClient(baseURL, clusterToken string, opts ...ClientOption) (*Client, error) {
	trimmedURL := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmedURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: orbit baseURL must be a valid http or https URL", domain.ErrInvalidConfig)
	}
	trimmedToken := strings.TrimSpace(clusterToken)
	if len(trimmedToken) < ticket.MinSecretLength {
		return nil, fmt.Errorf("%w: clusterToken must be at least %d characters", domain.ErrInvalidConfig, ticket.MinSecretLength)
	}

	c := &Client{
		baseURL:      trimmedURL,
		clusterToken: trimmedToken,
		httpClient:   &http.Client{Timeout: 10 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// RegisterCluster registers the cluster with POST /api/v1/machine_clusters/register.
func (c *Client) RegisterCluster(ctx context.Context, req protocol.RegisterClusterRequest) (protocol.RegisterClusterResponse, error) {
	var resp protocol.RegisterClusterResponse
	if err := c.postJSON(ctx, "/api/v1/machine_clusters/register", req, &resp); err != nil {
		return protocol.RegisterClusterResponse{}, err
	}
	return resp, nil
}

// SendHeartbeat sends active Pod status to POST /api/v1/machine_clusters/heartbeat.
func (c *Client) SendHeartbeat(ctx context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	var resp protocol.HeartbeatResponse
	if err := c.postJSON(ctx, "/api/v1/machine_clusters/heartbeat", req, &resp); err != nil {
		return protocol.HeartbeatResponse{}, err
	}
	return resp, nil
}

// MintCredentials requests short-lived MCP and Git credentials from POST /api/v1/machine_clusters/credentials.
func (c *Client) MintCredentials(ctx context.Context, req protocol.MintCredentialsRequest) (protocol.MintCredentialsResponse, error) {
	var resp protocol.MintCredentialsResponse
	if err := c.postJSON(ctx, "/api/v1/machine_clusters/credentials", req, &resp); err != nil {
		return protocol.MintCredentialsResponse{}, err
	}
	return resp, nil
}

func (c *Client) postJSON(ctx context.Context, path string, payload any, out any) error {
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(rawBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.clusterToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("orbit control plane POST %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding orbit control plane response: %w", err)
	}
	return nil
}

// FakeClient is an in-memory ControlPlaneClient for unit and integration tests.
type FakeClient struct {
	mu                sync.Mutex
	Heartbeats        []protocol.HeartbeatRequest
	Registrations     []protocol.RegisterClusterRequest
	CredentialMints   []protocol.MintCredentialsRequest
	RevokeOnHeartbeat []string
	FailSubject       string
}

// NewFakeClient creates a new in-memory FakeClient.
func NewFakeClient() *FakeClient {
	return &FakeClient{}
}

// RegisterCluster records the registration request and returns an active cluster response.
func (f *FakeClient) RegisterCluster(_ context.Context, req protocol.RegisterClusterRequest) (protocol.RegisterClusterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Registrations = append(f.Registrations, req)
	return protocol.RegisterClusterResponse{
		ClusterID:                "mc_fake",
		WorkspaceID:              "ws_fake",
		WorkspaceSlug:            "fake-ws",
		Status:                   "active",
		HeartbeatIntervalSeconds: 30,
	}, nil
}

// SendHeartbeat records the heartbeat and returns configured RevokeOnHeartbeat subjects.
func (f *FakeClient) SendHeartbeat(_ context.Context, req protocol.HeartbeatRequest) (protocol.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Heartbeats = append(f.Heartbeats, req)
	revoked := append([]string(nil), f.RevokeOnHeartbeat...)
	return protocol.HeartbeatResponse{
		Status:         "ok",
		RevokeSessions: revoked,
	}, nil
}

// MintCredentials records the credential request and returns synthetic credentials.
func (f *FakeClient) MintCredentials(_ context.Context, req protocol.MintCredentialsRequest) (protocol.MintCredentialsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CredentialMints = append(f.CredentialMints, req)
	if f.FailSubject != "" && req.Subject == f.FailSubject {
		return protocol.MintCredentialsResponse{}, errors.New("subject not found")
	}
	return protocol.MintCredentialsResponse{
		OrbitMCPToken: "omcp_fake_token",
		Repositories: []protocol.RepoCredential{
			{
				ID:            "repo_fake",
				Name:          "orbit",
				CloneURL:      "https://github.com/acme/orbit.git",
				DefaultBranch: "main",
			},
		},
	}, nil
}
