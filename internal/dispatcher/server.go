// Package dispatcher implements the HTTP/WebSocket session gateway and
// control-plane heartbeat loop for orbit-dispatcher.
package dispatcher

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/k8s"
	"github.com/davidschrooten/orbit-machines/internal/orbitclient"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/ticket"
	"golang.org/x/sync/errgroup"
)

// ServerConfig holds dependencies for the dispatcher Server.
type ServerConfig struct {
	ClusterToken      string
	MaxConcurrentPods int
	TicketCodec       *ticket.Codec
	FrameCodec        *protocol.FrameCodec
	Orchestrator      k8s.PodOrchestrator
	OrbitClient       orbitclient.ControlPlaneClient
	Now               func() time.Time
}

// Server is the HTTP/WebSocket session gateway and heartbeat runner.
type Server struct {
	clusterToken      string
	maxConcurrentPods int
	ticketCodec       *ticket.Codec
	frameCodec        *protocol.FrameCodec
	orchestrator      k8s.PodOrchestrator
	orbitClient       orbitclient.ControlPlaneClient
	now               func() time.Time

	mu       sync.Mutex
	sessions map[string]context.CancelFunc
}

// NewServer validates cfg and constructs a Server.
func NewServer(cfg ServerConfig) (*Server, error) {
	if len(strings.TrimSpace(cfg.ClusterToken)) < ticket.MinSecretLength {
		return nil, fmt.Errorf("%w: clusterToken is required", domain.ErrInvalidConfig)
	}
	if cfg.TicketCodec == nil || cfg.FrameCodec == nil || cfg.Orchestrator == nil {
		return nil, fmt.Errorf("%w: TicketCodec, FrameCodec, and Orchestrator are required", domain.ErrInvalidConfig)
	}
	maxPods := cfg.MaxConcurrentPods
	if maxPods <= 0 {
		maxPods = 25
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Server{
		clusterToken:      strings.TrimSpace(cfg.ClusterToken),
		maxConcurrentPods: maxPods,
		ticketCodec:       cfg.TicketCodec,
		frameCodec:        cfg.FrameCodec,
		orchestrator:      cfg.Orchestrator,
		orbitClient:       cfg.OrbitClient,
		now:               nowFn,
		sessions:          make(map[string]context.CancelFunc),
	}, nil
}

// Handler returns the http.Handler serving /healthz, /readyz, /ws/terminal, and /api/v1/sessions/{ws_id}/{sub}.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleHealthz)
	mux.HandleFunc("GET /ws/terminal", s.handleTerminalWS)
	mux.HandleFunc("DELETE /api/v1/sessions/{ws_id}/{sub}", s.handleDeleteSession)
	return mux
}

// SendHeartbeatOnce collects active Pod status, sends a heartbeat to Orbit, and revokes any listed sessions.
func (s *Server) SendHeartbeatOnce(ctx context.Context) error {
	if s.orbitClient == nil {
		return nil
	}
	active, err := s.orchestrator.ListActivePods(ctx)
	if err != nil {
		return err
	}

	statuses := make([]protocol.SessionStatus, 0, len(active))
	for _, p := range active {
		statuses = append(statuses, protocol.SessionStatus{
			WorkspaceID: p.MachineID.WorkspaceID(),
			Subject:     p.MachineID.UserID(),
			PodName:     p.PodName,
			Phase:       p.Phase,
			Tier:        p.Tier,
			IdleSeconds: p.IdleSeconds,
		})
	}

	resp, err := s.orbitClient.SendHeartbeat(ctx, protocol.HeartbeatRequest{
		ActivePods:        len(active),
		MaxConcurrentPods: s.maxConcurrentPods,
		Sessions:          statuses,
	})
	if err != nil {
		return err
	}

	for _, revoked := range resp.RevokeSessions {
		for _, p := range active {
			fullKey := p.MachineID.WorkspaceID() + "/" + p.MachineID.UserID()
			if revoked == p.MachineID.UserID() || revoked == fullKey {
				s.cancelActiveSession(p.MachineID)
				_ = s.orchestrator.DeletePod(ctx, p.MachineID)
			}
		}
	}
	return nil
}

// RunHeartbeatLoop periodically executes SendHeartbeatOnce until ctx is canceled.
func (s *Server) RunHeartbeatLoop(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	_ = s.SendHeartbeatOnce(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_ = s.SendHeartbeatOnce(ctx)
		}
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	rawTicket := strings.TrimSpace(r.URL.Query().Get("ticket"))
	claims, err := s.ticketCodec.Verify(rawTicket, s.now())
	if err != nil {
		http.Error(w, "invalid or expired session ticket", http.StatusUnauthorized)
		return
	}

	mid, _ := claims.MachineID()

	clientConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	clientConn.SetReadLimit(protocol.DefaultMaxPayloadBytes * 2)
	defer func() { _ = clientConn.CloseNow() }()

	sessCtx, sessCancel := context.WithCancel(r.Context())
	defer sessCancel()
	s.registerActiveSession(mid, sessCancel)
	defer s.unregisterActiveSession(mid)

	var mcpToken string
	if s.orbitClient != nil {
		if creds, credErr := s.orbitClient.MintCredentials(sessCtx, protocol.MintCredentialsRequest{
			WorkspaceID: mid.WorkspaceID(),
			Subject:     mid.UserID(),
		}); credErr == nil {
			mcpToken = creds.OrbitMCPToken
		}
	}

	if _, err := s.orchestrator.EnsurePod(sessCtx, k8s.PodRequest{
		MachineID:               mid,
		WorkspaceSlug:           claims.WorkspaceSlug,
		UserEmail:               claims.UserLogin,
		Tier:                    claims.Tier,
		ProfileEncryptionKeyHex: s.deriveProfileKeyHex(mid),
		OrbitMCPToken:           mcpToken,
	}); err != nil {
		_ = clientConn.Close(websocket.StatusTryAgainLater, err.Error())
		return
	}

	ep, err := s.orchestrator.WaitPodReady(sessCtx, mid, func(phase domain.BootPhase, message string, progressPct int) {
		metaFrame, encErr := s.frameCodec.EncodeMetadata(protocol.MetadataEvent{
			Type:        protocol.MetaEventBootProgress,
			Phase:       phase,
			Message:     message,
			ProgressPct: progressPct,
		})
		if encErr == nil {
			_ = clientConn.Write(sessCtx, websocket.MessageBinary, metaFrame)
		}
	})
	if err != nil {
		_ = clientConn.Close(websocket.StatusInternalError, "pod failed to become ready")
		return
	}

	podConn, err := dialPodWebSocket(sessCtx, ep.WebSocketURL)
	if err != nil {
		_ = clientConn.Close(websocket.StatusBadGateway, "unable to dial pod websocket")
		return
	}
	podConn.SetReadLimit(protocol.DefaultMaxPayloadBytes * 2)
	defer func() { _ = podConn.CloseNow() }()

	_ = spliceWebSockets(sessCtx, clientConn, podConn)
}

func (s *Server) deriveProfileKeyHex(mid domain.MachineID) string {
	mac := hmac.New(sha256.New, []byte(s.clusterToken))
	_, _ = mac.Write([]byte("orbit-profile-key:v1:" + mid.PodName()))
	return hex.EncodeToString(mac.Sum(nil))
}

func dialPodWebSocket(ctx context.Context, wsURL string) (*websocket.Conn, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		conn, _, err := websocket.Dial(ctx, wsURL, nil) //nolint:bodyclose // coder/websocket manages response body lifecycle
		if err == nil {
			return conn, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return nil, lastErr
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeBearer(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	mid, err := domain.NewMachineID(r.PathValue("ws_id"), r.PathValue("sub"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.cancelActiveSession(mid)
	if err := s.orchestrator.DeletePod(r.Context(), mid); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":   "terminated",
		"pod_name": mid.PodName(),
	})
}

func (s *Server) authorizeBearer(r *http.Request) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := strings.TrimSpace(auth[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.clusterToken)) == 1
}

func (s *Server) registerActiveSession(mid domain.MachineID, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mid.PodName()
	if prev, ok := s.sessions[key]; ok {
		prev()
	}
	s.sessions[key] = cancel
}

func (s *Server) unregisterActiveSession(mid domain.MachineID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, mid.PodName())
}

func (s *Server) cancelActiveSession(mid domain.MachineID) {
	s.mu.Lock()
	cancel, ok := s.sessions[mid.PodName()]
	delete(s.sessions, mid.PodName())
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

func spliceWebSockets(ctx context.Context, a, b *websocket.Conn) error {
	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return pumpWebSocket(gCtx, a, b)
	})
	g.Go(func() error {
		return pumpWebSocket(gCtx, b, a)
	})
	return g.Wait()
}

func pumpWebSocket(ctx context.Context, src, dst *websocket.Conn) error {
	for {
		msgType, data, err := src.Read(ctx)
		if err != nil {
			return err
		}
		if err := dst.Write(ctx, msgType, data); err != nil {
			return err
		}
	}
}
