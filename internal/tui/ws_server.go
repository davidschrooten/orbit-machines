package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/pty"
)

const (
	defaultWSReadLimit       = 64 << 20 // 64 MiB
	defaultKeepaliveInterval = 20 * time.Second
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
	Compositor        *Compositor
	ShellSpawner      ShellSpawnerFunc
	WorkspaceDir      string
	TickInterval      time.Duration // Defaults to 16ms (~60fps)
	KeepaliveInterval time.Duration // Defaults to 20s
	OnActivity        func()
	OnContextUpdate   func(req ContextUpdateRequest) error
	OnSnapshot        func(ctx context.Context) ([]string, error)
}

// WSServerOption configures a WSServer.
type WSServerOption func(*WSServer)

// WithWorkspaceDir overrides the target directory where uploaded files are written.
func WithWorkspaceDir(dir string) WSServerOption {
	return func(s *WSServer) {
		if trimmed := strings.TrimSpace(dir); trimmed != "" {
			s.workspaceDir = trimmed
		}
	}
}

type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *wsClient) WriteBinary(ctx context.Context, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageBinary, data)
}

type tabSession struct {
	id    string
	pane  *pty.Pane
	mu    sync.Mutex
	conns map[*wsClient]struct{}
}

// WSServer streams zstd-compressed terminal frames over binary WebSockets.
type WSServer struct {
	comp              *Compositor
	shellSpawner      ShellSpawnerFunc
	workspaceDir      string
	codec             *protocol.FrameCodec
	tickInterval      time.Duration
	keepaliveInterval time.Duration
	onActivity        func()
	onContextUpdate   func(req ContextUpdateRequest) error
	onSnapshot        func(ctx context.Context) ([]string, error)

	mu      sync.Mutex
	clients map[*wsClient]struct{}
	tabs    map[string]*tabSession
}

// NewWSServer validates cfg and constructs a WSServer.
func NewWSServer(cfg WSServerConfig, opts ...WSServerOption) (*WSServer, error) {
	if cfg.Compositor == nil && cfg.ShellSpawner == nil {
		return nil, fmt.Errorf("%w: Compositor or ShellSpawner is required", domain.ErrInvalidConfig)
	}
	codec, _ := protocol.NewFrameCodec()
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = 16 * time.Millisecond
	}
	keepalive := cfg.KeepaliveInterval
	if keepalive <= 0 {
		keepalive = defaultKeepaliveInterval
	}
	wsDir := strings.TrimSpace(cfg.WorkspaceDir)
	if wsDir == "" {
		if envWork := strings.TrimSpace(os.Getenv("ORBIT_MACHINED_WORK_DIR")); envWork != "" {
			wsDir = envWork
		} else if envHome := strings.TrimSpace(os.Getenv("HOME")); envHome != "" {
			wsDir = filepath.Join(envHome, "workspace")
		} else {
			wsDir = "/home/orbit/workspace"
		}
	}
	srv := &WSServer{
		comp:              cfg.Compositor,
		shellSpawner:      cfg.ShellSpawner,
		workspaceDir:      wsDir,
		codec:             codec,
		tickInterval:      tick,
		keepaliveInterval: keepalive,
		onActivity:        cfg.OnActivity,
		onContextUpdate:   cfg.OnContextUpdate,
		onSnapshot:        cfg.OnSnapshot,
		clients:           make(map[*wsClient]struct{}),
		tabs:              make(map[string]*tabSession),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(srv)
		}
	}
	return srv, nil
}

// Handler returns the HTTP handler serving GET /healthz, GET /ws, DELETE /api/v1/tabs/{tab_id},
// POST /api/v1/context, POST /api/v1/snapshot, and POST /api/v1/upload.
func (s *WSServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("DELETE /api/v1/tabs/{tab_id}", s.handleDeleteTab)
	mux.HandleFunc("POST /api/v1/context", s.handleUpdateContext)
	mux.HandleFunc("POST /api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /api/v1/upload", s.handleUpload)
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
	conns := make([]*wsClient, 0, len(ts.conns))
	for c := range ts.conns {
		conns = append(conns, c)
	}
	ts.conns = make(map[*wsClient]struct{})
	ts.mu.Unlock()

	for _, c := range conns {
		_ = c.conn.CloseNow()
	}
}

// BroadcastMetadata encodes ev as a 0x13 FrameServerMetadata and sends it to all connected clients.
func (s *WSServer) BroadcastMetadata(ev protocol.MetadataEvent) error {
	frameBytes, err := s.codec.EncodeMetadata(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	conns := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.WriteBinary(ctx, frameBytes)
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

func (s *WSServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	var uploaded []string

	if strings.HasPrefix(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(defaultWSReadLimit); err != nil || r.MultipartForm == nil {
			http.Error(w, "invalid multipart form", http.StatusBadRequest)
			return
		}
		for _, headers := range r.MultipartForm.File {
			for _, fh := range headers {
				f, err := fh.Open()
				if err != nil {
					continue
				}
				data, _ := io.ReadAll(f)
				_ = f.Close()
				destPath, saveErr := s.saveUploadedFile(fh.Filename, data)
				if saveErr != nil {
					http.Error(w, saveErr.Error(), http.StatusBadRequest)
					return
				}
				uploaded = append(uploaded, destPath)
			}
		}
		if len(uploaded) == 0 {
			http.Error(w, "no files provided in multipart upload", http.StatusBadRequest)
			return
		}
	} else {
		body, _ := io.ReadAll(io.LimitReader(r.Body, defaultWSReadLimit))
		name, data, err := protocol.DecodeUploadPayload(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		destPath, err := s.saveUploadedFile(name, data)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		uploaded = append(uploaded, destPath)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"uploaded": uploaded,
	})
}

func sanitizeUploadFilename(name string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	safeName := filepath.Base(filepath.Clean(trimmed))
	if trimmed == "" || safeName == "." || safeName == ".." || safeName == "/" {
		return "", fmt.Errorf("%w: invalid upload filename %q", domain.ErrInvalidConfig, name)
	}
	return safeName, nil
}

func (s *WSServer) saveUploadedFile(name string, data []byte) (string, error) {
	safeName, err := sanitizeUploadFilename(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.workspaceDir, 0o755); err != nil {
		return "", fmt.Errorf("creating workspace directory: %w", err)
	}
	destPath := filepath.Join(s.workspaceDir, safeName)
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return "", fmt.Errorf("writing uploaded file: %w", err)
	}
	return destPath, nil
}

func (s *WSServer) handleFileUploadFrame(ctx context.Context, client *wsClient, payload []byte) {
	name, data, err := protocol.DecodeUploadPayload(payload)
	if err != nil {
		return
	}
	destPath, err := s.saveUploadedFile(name, data)
	if err != nil {
		return
	}
	safeName := filepath.Base(destPath)
	statusLine := fmt.Sprintf("\r\n\x1b[32m✓ Uploaded %s (%d bytes) to %s\x1b[0m\r\n", safeName, len(data), destPath)
	if diffBytes, encErr := s.codec.EncodeDiff([]byte(statusLine)); encErr == nil {
		_ = client.WriteBinary(ctx, diffBytes)
	}
}

func (s *WSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	conn.SetReadLimit(defaultWSReadLimit)
	defer func() { _ = conn.CloseNow() }()

	client := &wsClient{conn: conn}

	s.mu.Lock()
	s.clients[client] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.clients, client)
		s.mu.Unlock()
	}()

	if s.shellSpawner != nil {
		s.handleShellWS(r, client)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 1. Immediately send a full screen snapshot (0x10) on initial connect/reconnect.
	_ = s.comp.ConsumeDirty()
	initialSnap, _ := s.codec.EncodeSnapshot([]byte(s.comp.View()))
	if err := client.WriteBinary(ctx, initialSnap); err != nil {
		return
	}

	// 2. Start 60fps dirty frame coalescer ticker goroutine.
	go s.renderLoop(ctx, client)

	// 3. Read client binary frames (0x01 Input, 0x02 Resize, 0x03 Ping, 0x04 FileUpload).
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
			_ = client.WriteBinary(ctx, snapBytes)
		case protocol.FramePing:
			pongBytes, _ := s.codec.EncodePong(frame.TimestampMS)
			_ = client.WriteBinary(ctx, pongBytes)
		case protocol.FrameFileUpload:
			if s.onActivity != nil {
				s.onActivity()
			}
			s.handleFileUploadFrame(ctx, client, frame.Data)
		}
	}
}

func (s *WSServer) handleShellWS(r *http.Request, client *wsClient) {
	tabID := strings.TrimSpace(r.URL.Query().Get("tab"))
	if tabID == "" {
		tabID = "tab-1"
	}

	ts, err := s.getOrSpawnTab(tabID)
	if err != nil {
		_ = client.conn.Close(websocket.StatusInternalError, err.Error())
		return
	}

	ts.mu.Lock()
	ts.conns[client] = struct{}{}
	ts.mu.Unlock()
	defer func() {
		ts.mu.Lock()
		delete(ts.conns, client)
		ts.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Immediately replay the tab's raw scrollback buffer as a 0x10 FrameSnapshot.
	initialSnap, _ := s.codec.EncodeSnapshot(ts.pane.Scrollback())
	if err := client.WriteBinary(ctx, initialSnap); err != nil {
		return
	}

	// Start periodic 20s FramePong keepalive so intermediate proxies/ingresses do not drop idle shells.
	go s.keepaliveLoop(ctx, client)

	for {
		_, raw, err := client.conn.Read(ctx)
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
			_ = client.WriteBinary(ctx, pongBytes)
		case protocol.FrameFileUpload:
			if s.onActivity != nil {
				s.onActivity()
			}
			s.handleFileUploadFrame(ctx, client, frame.Data)
		}
	}
}

func (s *WSServer) keepaliveLoop(ctx context.Context, client *wsClient) {
	ticker := time.NewTicker(s.keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			pongBytes, _ := s.codec.EncodePong(uint64(now.UnixMilli()))
			if err := client.WriteBinary(ctx, pongBytes); err != nil {
				return
			}
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
		conns: make(map[*wsClient]struct{}),
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
	conns := make([]*wsClient, 0, len(ts.conns))
	for c := range ts.conns {
		conns = append(conns, c)
	}
	ts.mu.Unlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.WriteBinary(ctx, diffBytes)
		cancel()
	}
}

func (s *WSServer) renderLoop(ctx context.Context, client *wsClient) {
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
			if err := client.WriteBinary(ctx, diffBytes); err != nil {
				return
			}
		}
	}
}
