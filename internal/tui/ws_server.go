package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/pty"
)

// ShellSpawnerFunc spawns an interactive PTY shell pane for the given tab ID.
type ShellSpawnerFunc func(ctx context.Context, tabID string, cols, rows int) (*pty.Pane, error)

// ContextUpdateRequest is sent by orbit-dispatcher to POST /api/v1/context
// to refresh Orbit credentials and workspace repository metadata inside a running Pod.
type ContextUpdateRequest struct {
	OrbitURL      string                    `json:"orbit_url"`
	OrbitMCPToken string                    `json:"orbit_mcp_token"`
	WorkspaceID   string                    `json:"workspace_id"`
	WorkspaceSlug string                    `json:"workspace_slug"`
	UserID        string                    `json:"user_id"`
	UserEmail     string                    `json:"user_email"`
	Repositories  []protocol.RepoCredential `json:"repositories"`
}

// WSServerConfig configures the in-Pod WebSocket server (:8080/ws).
type WSServerConfig struct {
	Compositor      *Compositor
	ShellSpawner    ShellSpawnerFunc
	TickInterval    time.Duration // Defaults to 16ms (~60fps)
	OnActivity      func()
	OnContextUpdate func(req ContextUpdateRequest) error
	OnSnapshot      func(ctx context.Context) ([]string, error)
}

type tabSession struct {
	id    string
	pane  *pty.Pane
	mu    sync.Mutex
	conns map[*websocket.Conn]struct{}
}

// WSServer streams zstd-compressed terminal frames over binary WebSockets.
type WSServer struct {
	comp            *Compositor
	shellSpawner    ShellSpawnerFunc
	codec           *protocol.FrameCodec
	tickInterval    time.Duration
	onActivity      func()
	onContextUpdate func(req ContextUpdateRequest) error
	onSnapshot      func(ctx context.Context) ([]string, error)

	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
	tabs    map[string]*tabSession
}

// NewWSServer validates cfg and constructs a WSServer.
func NewWSServer(cfg WSServerConfig) (*WSServer, error) {
	if cfg.Compositor == nil && cfg.ShellSpawner == nil {
		return nil, fmt.Errorf("%w: Compositor or ShellSpawner is required", domain.ErrInvalidConfig)
	}
	codec, _ := protocol.NewFrameCodec()
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = 16 * time.Millisecond
	}
	return &WSServer{
		comp:            cfg.Compositor,
		shellSpawner:    cfg.ShellSpawner,
		codec:           codec,
		tickInterval:    tick,
		onActivity:      cfg.OnActivity,
		onContextUpdate: cfg.OnContextUpdate,
		onSnapshot:      cfg.OnSnapshot,
		clients:         make(map[*websocket.Conn]struct{}),
		tabs:            make(map[string]*tabSession),
	}, nil
}

// Handler returns the HTTP handler serving GET /healthz, GET /ws, DELETE /api/v1/tabs/{tab_id},
// POST /api/v1/context, and POST /api/v1/snapshot.
func (s *WSServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("DELETE /api/v1/tabs/{tab_id}", s.handleDeleteTab)
	mux.HandleFunc("POST /api/v1/context", s.handleUpdateContext)
	mux.HandleFunc("POST /api/v1/snapshot", s.handleSnapshot)
	return mux
}

// Close terminates all active tab PTY sessions.
func (s *WSServer) Close() error {
	s.mu.Lock()
	tabs := make([]*tabSession, 0, len(s.tabs))
	for _, ts := range s.tabs {
		tabs = append(tabs, ts)
	}
	s.tabs = make(map[string]*tabSession)
	s.mu.Unlock()

	for _, ts := range tabs {
		_ = ts.pane.Close()
	}
	return nil
}

// CloseTab terminates and removes the PTY session for tabID.
func (s *WSServer) CloseTab(tabID string) {
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return
	}
	s.mu.Lock()
	ts, ok := s.tabs[tabID]
	delete(s.tabs, tabID)
	s.mu.Unlock()

	if !ok || ts == nil {
		return
	}
	_ = ts.pane.Close()

	ts.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(ts.conns))
	for c := range ts.conns {
		conns = append(conns, c)
	}
	ts.conns = make(map[*websocket.Conn]struct{})
	ts.mu.Unlock()

	for _, c := range conns {
		_ = c.CloseNow()
	}
}

// BroadcastMetadata encodes ev as a 0x13 FrameServerMetadata and sends it to all connected clients.
func (s *WSServer) BroadcastMetadata(ev protocol.MetadataEvent) error {
	frameBytes, err := s.codec.EncodeMetadata(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.clients))
	for c := range s.clients {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Write(ctx, websocket.MessageBinary, frameBytes)
		cancel()
	}
	return nil
}

func (s *WSServer) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *WSServer) handleDeleteTab(w http.ResponseWriter, r *http.Request) {
	tabID := strings.TrimSpace(r.PathValue("tab_id"))
	s.CloseTab(tabID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "closed",
		"tab_id": tabID,
	})
}

func (s *WSServer) handleUpdateContext(w http.ResponseWriter, r *http.Request) {
	var req ContextUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}
	if s.onContextUpdate != nil {
		if err := s.onContextUpdate(req); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"updated"}`))
}

func (s *WSServer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	var repos []string
	if s.onSnapshot != nil {
		snapRepos, err := s.onSnapshot(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		repos = snapRepos
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "snapshotted",
		"repositories": repos,
	})
}

func (s *WSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()

	s.mu.Lock()
	s.clients[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, conn)
		s.mu.Unlock()
	}()

	if s.shellSpawner != nil {
		s.handleTabShellWS(r, conn)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 1. Immediately send a full screen snapshot (0x10) on initial connect/reconnect.
	_ = s.comp.ConsumeDirty()
	initialSnap, _ := s.codec.EncodeSnapshot([]byte(s.comp.View()))
	if err := conn.Write(ctx, websocket.MessageBinary, initialSnap); err != nil {
		return
	}

	// 2. Start 60fps dirty frame coalescer ticker goroutine.
	go s.renderLoop(ctx, conn)

	// 3. Read client binary frames (0x01 Input, 0x02 Resize, 0x03 Ping).
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		frame, err := s.codec.Decode(raw)
		if err != nil {
			continue
		}

		switch frame.Type {
		case protocol.FrameInput:
			if s.onActivity != nil {
				s.onActivity()
			}
			s.comp.HandleRawInput(frame.Data)
		case protocol.FrameResize:
			if s.onActivity != nil {
				s.onActivity()
			}
			s.comp.Resize(int(frame.Cols), int(frame.Rows))
			_ = s.comp.ConsumeDirty()
			snapBytes, _ := s.codec.EncodeSnapshot([]byte(s.comp.View()))
			_ = conn.Write(ctx, websocket.MessageBinary, snapBytes)
		case protocol.FramePing:
			pongBytes, _ := s.codec.EncodePong(frame.TimestampMS)
			_ = conn.Write(ctx, websocket.MessageBinary, pongBytes)
		}
	}
}

func (s *WSServer) handleTabShellWS(r *http.Request, conn *websocket.Conn) {
	tabID := strings.TrimSpace(r.URL.Query().Get("tab"))
	if tabID == "" {
		tabID = "tab-1"
	}

	ts, err := s.getOrSpawnTab(tabID)
	if err != nil {
		_ = conn.Close(websocket.StatusInternalError, err.Error())
		return
	}

	ts.mu.Lock()
	ts.conns[conn] = struct{}{}
	ts.mu.Unlock()
	defer func() {
		ts.mu.Lock()
		delete(ts.conns, conn)
		ts.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Immediately replay the tab's raw scrollback buffer as a 0x10 FrameSnapshot.
	initialSnap, _ := s.codec.EncodeSnapshot(ts.pane.Scrollback())
	if err := conn.Write(ctx, websocket.MessageBinary, initialSnap); err != nil {
		return
	}

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == 4000 || strings.Contains(err.Error(), "close-tab") {
				s.CloseTab(tabID)
			}
			return
		}
		frame, err := s.codec.Decode(raw)
		if err != nil {
			continue
		}

		switch frame.Type {
		case protocol.FrameInput:
			if s.onActivity != nil {
				s.onActivity()
			}
			_, _ = ts.pane.WriteInput(frame.Data)
		case protocol.FrameResize:
			if s.onActivity != nil {
				s.onActivity()
			}
			_ = ts.pane.Resize(int(frame.Cols), int(frame.Rows))
		case protocol.FramePing:
			pongBytes, _ := s.codec.EncodePong(frame.TimestampMS)
			_ = conn.Write(ctx, websocket.MessageBinary, pongBytes)
		}
	}
}

func (s *WSServer) getOrSpawnTab(tabID string) (*tabSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.tabs[tabID]; ok && existing != nil {
		if !existing.pane.Exited() {
			return existing, nil
		}
		_ = existing.pane.Close()
		delete(s.tabs, tabID)
	}

	pane, err := s.shellSpawner(context.Background(), tabID, 120, 36)
	if err != nil {
		return nil, err
	}
	ts := &tabSession{
		id:    tabID,
		pane:  pane,
		conns: make(map[*websocket.Conn]struct{}),
	}
	pane.SetOnOutput(func(chunk []byte) {
		s.broadcastTabOutput(ts, chunk)
	})
	s.tabs[tabID] = ts
	return ts, nil
}

func (s *WSServer) broadcastTabOutput(ts *tabSession, chunk []byte) {
	diffBytes, err := s.codec.EncodeDiff(chunk)
	if err != nil {
		return
	}
	ts.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(ts.conns))
	for c := range ts.conns {
		conns = append(conns, c)
	}
	ts.mu.Unlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.Write(ctx, websocket.MessageBinary, diffBytes)
		cancel()
	}
}

func (s *WSServer) renderLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(s.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.comp.ConsumeDirty() {
				continue
			}
			diffBytes, _ := s.codec.EncodeDiff([]byte(s.comp.View()))
			if err := conn.Write(ctx, websocket.MessageBinary, diffBytes); err != nil {
				return
			}
		}
	}
}
