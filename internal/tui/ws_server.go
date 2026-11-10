package tui

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
			s.persistTabs = true
		}
	}
}

type persistedTabsState struct {
	ActiveTabs []string  `json:"active_tabs"`
	UpdatedAt  time.Time `json:"updated_at"`
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
	persistTabs       bool
	codec             *protocol.FrameCodec
	tickInterval      time.Duration
	keepaliveInterval time.Duration
	onActivity        func()
	onContextUpdate   func(req ContextUpdateRequest) error
	onSnapshot        func(ctx context.Context) ([]string, error)

	mu            sync.Mutex
	clients       map[*wsClient]struct{}
	tabs          map[string]*tabSession
	persistedTabs map[string]struct{}
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
	persistTabs := wsDir != ""
	if wsDir == "" {
		if envWork := strings.TrimSpace(os.Getenv("ORBIT_MACHINED_WORK_DIR")); envWork != "" {
			wsDir = envWork
			persistTabs = true
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
		persistTabs:       persistTabs,
		codec:             codec,
		tickInterval:      tick,
		keepaliveInterval: keepalive,
		onActivity:        cfg.OnActivity,
		onContextUpdate:   cfg.OnContextUpdate,
		onSnapshot:        cfg.OnSnapshot,
		clients:           make(map[*wsClient]struct{}),
		tabs:              make(map[string]*tabSession),
		persistedTabs:     make(map[string]struct{}),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(srv)
		}
	}
	srv.loadPersistedTabsLocked()
	return srv, nil
}

// Handler returns the HTTP handler serving GET /healthz, GET /ws, GET /api/v1/tabs,
// DELETE /api/v1/tabs/{tab_id}, POST /api/v1/context, POST /api/v1/snapshot, and POST /api/v1/upload.
func (s *WSServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /api/v1/tabs", s.handleListTabs)
	mux.HandleFunc("DELETE /api/v1/tabs/{tab_id}", s.handleDeleteTab)
	mux.HandleFunc("POST /api/v1/context", s.handleUpdateContext)
	mux.HandleFunc("POST /api/v1/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /api/v1/upload", s.handleUpload)
	return mux
}

// ActiveTabIDs returns all active and persisted tab IDs sorted by numeric suffix ("tab-1", "tab-2", ...).
func (s *WSServer) ActiveTabIDs() []string {
	s.mu.Lock()
	seen := make(map[string]struct{}, len(s.tabs)+len(s.persistedTabs))
	for id, ts := range s.tabs {
		if ts != nil && !ts.pane.Exited() {
			seen[id] = struct{}{}
		}
	}
	for id := range s.persistedTabs {
		seen[id] = struct{}{}
	}
	s.mu.Unlock()

	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sortTabIDs(out)
	return out
}

// Close terminates all active tab PTY sessions after flushing their scrollback state to disk.
func (s *WSServer) Close() error {
	s.flushTabsToDisk()

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
	tabID = sanitizeTabID(tabID)
	if tabID == "" {
		return
	}
	s.mu.Lock()
	ts, ok := s.tabs[tabID]
	delete(s.tabs, tabID)
	_, wasPersisted := s.persistedTabs[tabID]
	delete(s.persistedTabs, tabID)
	s.mu.Unlock()

	s.removeTabScrollback(tabID)
	s.saveTabsState()

	if ok && ts != nil {
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

	if ok || wasPersisted {
		_ = s.BroadcastMetadata(protocol.MetadataEvent{
			Type:        protocol.MetaEventSessionState,
			Phase:       domain.BootPhaseReady,
			ActiveTabs:  s.ActiveTabIDs(),
			ClosedTabID: tabID,
		})
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

func (s *WSServer) handleListTabs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"active_tabs": s.ActiveTabIDs(),
	})
}

func (s *WSServer) handleDeleteTab(w http.ResponseWriter, r *http.Request) {
	tabID := strings.TrimSpace(r.PathValue("tab_id"))
	s.CloseTab(tabID)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "closed",
		"tab_id":      tabID,
		"active_tabs": s.ActiveTabIDs(),
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
	s.flushTabsToDisk()

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
	tabID := sanitizeTabID(r.URL.Query().Get("tab"))
	if tabID == "" {
		tabID = "tab-1"
	}

	ts, created, err := s.getOrSpawnTab(tabID)
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
	scrollback := ts.pane.Scrollback()
	initialSnap, _ := s.codec.EncodeSnapshot(scrollback)
	if err := client.WriteBinary(ctx, initialSnap); err != nil {
		return
	}
	if len(scrollback) > 0 {
		cols, rows := ts.pane.Size()
		_ = ts.pane.Resize(cols, rows)
	}

	// Send current active_tabs session state to the connecting client and notify other clients if a new tab was spawned.
	activeTabs := s.ActiveTabIDs()
	if metaBytes, encErr := s.codec.EncodeMetadata(protocol.MetadataEvent{
		Type:       protocol.MetaEventSessionState,
		Phase:      domain.BootPhaseReady,
		ActiveTabs: activeTabs,
	}); encErr == nil {
		_ = client.WriteBinary(ctx, metaBytes)
		if created {
			s.broadcastFrameExcept(metaBytes, client)
		}
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

func (s *WSServer) getOrSpawnTab(tabID string) (*tabSession, bool, error) {
	tabID = sanitizeTabID(tabID)
	if tabID == "" {
		tabID = "tab-1"
	}

	s.mu.Lock()
	if existing, ok := s.tabs[tabID]; ok && existing != nil {
		if !existing.pane.Exited() {
			s.persistedTabs[tabID] = struct{}{}
			s.mu.Unlock()
			return existing, false, nil
		}
		_ = existing.pane.Close()
		delete(s.tabs, tabID)
	}

	var savedScrollback []byte
	if s.persistTabs {
		savedScrollback, _ = os.ReadFile(s.tabScrollbackPath(tabID))
	}

	pane, err := s.shellSpawner(context.Background(), tabID, 120, 36)
	if err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	if len(savedScrollback) > 0 {
		pane.PrependScrollback(savedScrollback)
	}

	_, wasPersisted := s.persistedTabs[tabID]
	s.persistedTabs[tabID] = struct{}{}
	ts := &tabSession{
		id:    tabID,
		pane:  pane,
		conns: make(map[*wsClient]struct{}),
	}
	pane.SetOnOutput(func(chunk []byte) {
		s.saveTabScrollback(tabID, ts.pane.Scrollback())
		s.broadcastTabOutput(ts, chunk)
	})
	s.tabs[tabID] = ts
	initialScrollback := pane.Scrollback()
	s.mu.Unlock()

	s.saveTabScrollback(tabID, initialScrollback)
	s.saveTabsState()
	return ts, !wasPersisted, nil
}

func (s *WSServer) broadcastFrameExcept(frameBytes []byte, skip *wsClient) {
	s.mu.Lock()
	conns := make([]*wsClient, 0, len(s.clients))
	for c := range s.clients {
		if c != skip {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()

	for _, c := range conns {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = c.WriteBinary(ctx, frameBytes)
		cancel()
	}
}

func (s *WSServer) tabsStatePath() string {
	return filepath.Join(s.workspaceDir, ".orbit", "tabs.json")
}

func (s *WSServer) tabScrollbackPath(tabID string) string {
	return filepath.Join(s.workspaceDir, ".orbit", "tabs", sanitizeTabID(tabID)+".scrollback")
}

func (s *WSServer) loadPersistedTabsLocked() {
	if !s.persistTabs {
		return
	}
	raw, err := os.ReadFile(s.tabsStatePath())
	if err != nil {
		return
	}
	var state persistedTabsState
	if err := json.Unmarshal(raw, &state); err != nil {
		return
	}
	for _, id := range state.ActiveTabs {
		if clean := sanitizeTabID(id); clean != "" {
			s.persistedTabs[clean] = struct{}{}
		}
	}
}

func (s *WSServer) saveTabsState() {
	if !s.persistTabs {
		return
	}
	state := persistedTabsState{
		ActiveTabs: s.ActiveTabIDs(),
		UpdatedAt:  time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	orbitDir := filepath.Join(s.workspaceDir, ".orbit")
	if err := os.MkdirAll(orbitDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(s.tabsStatePath(), raw, 0o644)
}

func (s *WSServer) saveTabScrollback(tabID string, data []byte) {
	if !s.persistTabs || len(data) == 0 {
		return
	}
	clean := sanitizeTabID(tabID)
	if clean == "" {
		return
	}
	tabsDir := filepath.Join(s.workspaceDir, ".orbit", "tabs")
	if err := os.MkdirAll(tabsDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(s.tabScrollbackPath(clean), data, 0o644)
}

func (s *WSServer) removeTabScrollback(tabID string) {
	if !s.persistTabs {
		return
	}
	clean := sanitizeTabID(tabID)
	if clean == "" {
		return
	}
	_ = os.Remove(s.tabScrollbackPath(clean))
}

func (s *WSServer) flushTabsToDisk() {
	if !s.persistTabs {
		return
	}
	s.mu.Lock()
	snapshots := make(map[string][]byte, len(s.tabs))
	for id, ts := range s.tabs {
		if ts != nil && !ts.pane.Exited() {
			snapshots[id] = ts.pane.Scrollback()
		}
	}
	s.mu.Unlock()

	for id, scrollback := range snapshots {
		s.saveTabScrollback(id, scrollback)
	}
	s.saveTabsState()
}

func sanitizeTabID(tabID string) string {
	trimmed := strings.TrimSpace(tabID)
	if trimmed == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sortTabIDs(ids []string) {
	slices.SortFunc(ids, func(a, b string) int {
		na, oka := parseTabNumber(a)
		nb, okb := parseTabNumber(b)
		if oka && okb {
			return cmp.Compare(na, nb)
		}
		if oka {
			return -1
		}
		if okb {
			return 1
		}
		return cmp.Compare(a, b)
	})
}

func parseTabNumber(tabID string) (int, bool) {
	if !strings.HasPrefix(tabID, "tab-") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(tabID, "tab-"))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
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
