package tui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/coder/websocket"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/pty"
	"github.com/davidschrooten/orbit-machines/internal/tui"
)

func TestCompositor_LeaderChordsSplitZoomFocusTabsAndModals(t *testing.T) {
	t.Parallel()

	spawner := func(id, title string, cols, rows int) *pty.Pane {
		p := pty.NewVirtualPane(id, title, cols, rows)
		p.FeedOutput([]byte("Welcome to " + title + " (" + id + ")\r\n"))
		return p
	}

	comp := tui.NewCompositor(tui.CompositorConfig{
		WorkspaceSlug: "acme",
		ResourceTier:  "standard",
		Repositories: []tui.RepoTabConfig{
			{Name: "orbit", HasFlake: true},
			{Name: "legacy-service", HasFlake: false},
		},
		SpawnPane: spawner,
	})
	defer func() { _ = comp.Close() }()

	_ = comp.Init()

	// 1. Resize terminal to 120x36 via WindowSizeMsg.
	m, _ := comp.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
	comp = m.(*tui.Compositor)

	view := comp.View()
	if !strings.Contains(view, "1:orbit") || !strings.Contains(view, "2:legacy-service") {
		t.Fatalf("expected top bar with repo tabs in view, got:\n%s", view)
	}
	if !strings.Contains(view, "✦ Configure") {
		t.Fatalf("expected ✦ Configure button in top bar")
	}

	// 2. Typing normal keys when not in leader mode writes directly to focused pane.
	comp.HandleRawInput([]byte("ls -la\n"))
	focused := comp.FocusedPane()
	if focused == nil {
		t.Fatalf("expected initial focused pane")
	}
	if got := string(focused.RecordedInput()); got != "ls -la\n" {
		t.Fatalf("focused pane RecordedInput = %q, want %q", got, "ls -la\n")
	}

	// 3. Leader chord: Ctrl+Space (0x00) enters LeaderPending, 'v' splits vertically and spawns a 2nd pane.
	comp.HandleRawInput([]byte{0x00})
	if !comp.IsLeaderPending() {
		t.Fatalf("expected LeaderPending=true after 0x00")
	}
	if !strings.Contains(comp.View(), "LEADER") {
		t.Fatalf("expected LEADER indicator in HUD")
	}

	comp.HandleRawInput([]byte("v"))
	if comp.IsLeaderPending() {
		t.Fatalf("expected LeaderPending=false after chord 'v'")
	}
	if len(comp.ActivePanes()) != 2 {
		t.Fatalf("expected 2 panes after vertical split, got %d", len(comp.ActivePanes()))
	}
	if comp.SplitLayout() != tui.LayoutVertical {
		t.Fatalf("expected LayoutVertical, got %v", comp.SplitLayout())
	}
	// Render vertical multi-pane layout.
	if !strings.Contains(comp.View(), "pane-1") || !strings.Contains(comp.View(), "pane-2") {
		t.Fatalf("expected vertical split View() to render both panes")
	}

	// 4. Leader chord 'h' / 'l' moves focus between panes; 's' switches to horizontal split.
	secondPaneID := comp.FocusedPane().ID()
	comp.HandleRawInput([]byte{0x00, 'h'})
	if comp.FocusedPane().ID() == secondPaneID {
		t.Fatalf("expected focus to move to first pane on Ctrl+Space h")
	}
	comp.HandleRawInput([]byte{0x00, 'l'})
	if comp.FocusedPane().ID() != secondPaneID {
		t.Fatalf("expected focus to move back to second pane on Ctrl+Space l")
	}
	comp.HandleRawInput([]byte{0x00, 'j'})
	comp.HandleRawInput([]byte{0x00, 'k'})

	comp.HandleRawInput([]byte{0x00, 's'})
	if comp.SplitLayout() != tui.LayoutHorizontal {
		t.Fatalf("expected LayoutHorizontal after Ctrl+Space s")
	}
	// Render horizontal multi-pane layout.
	if !strings.Contains(comp.View(), "pane-3") {
		t.Fatalf("expected horizontal split View() to render pane-3")
	}

	// 5. Mouse split border drag clamps ratio to [0.2, 0.8] and resizes panes.
	comp.DragSplitBorder(0.15)
	if comp.SplitRatio() <= 0.5 {
		t.Fatalf("expected SplitRatio > 0.5 after positive drag, got %f", comp.SplitRatio())
	}
	comp.DragSplitBorder(10.0)
	if comp.SplitRatio() != 0.8 {
		t.Fatalf("expected SplitRatio clamped at 0.8, got %f", comp.SplitRatio())
	}
	comp.DragSplitBorder(-10.0)
	if comp.SplitRatio() != 0.2 {
		t.Fatalf("expected SplitRatio clamped at 0.2, got %f", comp.SplitRatio())
	}

	// 6. Leader chord 'z' toggles zoom on focused pane.
	comp.HandleRawInput([]byte{0x00, 'z'})
	if !comp.IsZoomed() {
		t.Fatalf("expected IsZoomed()=true after Ctrl+Space z")
	}
	if !strings.Contains(comp.View(), "ZOOM") {
		t.Fatalf("expected ZOOM indicator in view when zoomed")
	}
	comp.HandleRawInput([]byte{0x00, 'z'})
	if comp.IsZoomed() {
		t.Fatalf("expected IsZoomed()=false after second Ctrl+Space z")
	}

	// 7. Leader chords for modals: 'c' (Tool Launcher), 'r' (Repo Switcher), 'i' (Issue Picker), 'a' (AI Configurator).
	for _, tc := range []struct {
		key   byte
		modal tui.ModalKind
		label string
	}{
		{'c', tui.ModalToolLauncher, "Tool Launcher"},
		{'r', tui.ModalRepoSwitcher, "Repository Switcher"},
		{'i', tui.ModalIssuePicker, "Orbit Issue Picker"},
		{'a', tui.ModalAIConfigurator, "Configure through an AI"},
	} {
		comp.HandleRawInput([]byte{0x00, tc.key})
		if comp.ActiveModal() != tc.modal {
			t.Fatalf("key %q: ActiveModal = %v, want %v", tc.key, comp.ActiveModal(), tc.modal)
		}
		if !strings.Contains(comp.View(), tc.label) {
			t.Fatalf("key %q: expected view to contain modal label %q", tc.key, tc.label)
		}
		// Escape closes the active modal.
		comp.HandleRawInput([]byte{0x1b})
		if comp.ActiveModal() != tui.ModalNone {
			t.Fatalf("expected Escape (0x1b) to dismiss modal %v", tc.modal)
		}
	}

	// 8. Leader chord 'x' closes focused pane.
	comp.HandleRawInput([]byte{0x00, 'x'})
	if len(comp.ActivePanes()) != 2 { // 's' added a 3rd pane earlier, so closing 1 leaves 2
		t.Fatalf("expected 2 panes remaining after Ctrl+Space x, got %d", len(comp.ActivePanes()))
	}

	// 9. Leader chord '2' switches to repo tab 2 ("legacy-service", which has HasFlake=false -> opens ModalFlakeWizard!).
	comp.HandleRawInput([]byte{0x00, '2'})
	if comp.ActiveTabIndex() != 1 {
		t.Fatalf("expected ActiveTabIndex=1 after Ctrl+Space 2, got %d", comp.ActiveTabIndex())
	}
	if comp.ActiveModal() != tui.ModalFlakeWizard {
		t.Fatalf("expected ModalFlakeWizard when switching to repo without flake.nix, got %v", comp.ActiveModal())
	}
	if !strings.Contains(comp.View(), "Flake Init Wizard") {
		t.Fatalf("expected Flake Init Wizard overlay in View()")
	}

	// 10. OpenModal(ModalWIPRestore), custom SetModalRenderer, clamping Resize, and tea.KeyMsg.
	comp.OpenModal(tui.ModalWIPRestore)
	if !strings.Contains(comp.View(), "Restore Uncommitted WIP Bundle") {
		t.Fatalf("expected WIP Restore modal in View()")
	}
	comp.SetModalRenderer(tui.ModalWIPRestore, func(_, _ int) string {
		return "CUSTOM_WIP_MODAL"
	})
	if !strings.Contains(comp.View(), "CUSTOM_WIP_MODAL") {
		t.Fatalf("expected custom modal renderer in View()")
	}
	comp.Resize(5, 2)
	w, h := comp.Size()
	if w != 20 || h != 6 {
		t.Fatalf("expected Resize(5, 2) to clamp to (20, 6), got (%d, %d)", w, h)
	}
	_, _ = comp.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})

	// Default CompositorConfig fallbacks.
	defComp := tui.NewCompositor(tui.CompositorConfig{})
	_ = defComp.Close()
}

func TestWSServer_60fpsDiffCoalescingSnapshotPingAndMetadata(t *testing.T) {
	t.Parallel()

	comp := tui.NewCompositor(tui.CompositorConfig{
		WorkspaceSlug: "acme",
		ResourceTier:  "performance",
		Repositories: []tui.RepoTabConfig{
			{Name: "orbit", HasFlake: true},
		},
	})
	defer func() { _ = comp.Close() }()

	var activityCount int
	if _, err := tui.NewWSServer(tui.WSServerConfig{}); err == nil {
		t.Fatalf("expected error for nil Compositor in NewWSServer")
	}
	_, _ = tui.NewWSServer(tui.WSServerConfig{Compositor: comp}) // default TickInterval
	wsSrv, err := tui.NewWSServer(tui.WSServerConfig{
		Compositor:   comp,
		TickInterval: 16 * time.Millisecond,
		OnActivity: func() {
			activityCount++
		},
	})
	if err != nil {
		t.Fatalf("NewWSServer: %v", err)
	}

	httpSrv := httptest.NewServer(wsSrv.Handler())
	defer httpSrv.Close()

	// Non-WebSocket request to /ws fails upgrade cleanly.
	badWSResp, err := http.Get(httpSrv.URL + "/ws")
	if err == nil {
		_ = badWSResp.Body.Close()
	}

	// 1. Verify /healthz endpoint.
	resp, err := http.Get(httpSrv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// 2. Connect to /ws and verify immediate 0x10 FrameServerSnapshot on connect.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
	conn, _, err := websocket.Dial(ctx, wsURL, nil) //nolint:bodyclose // coder/websocket manages response body lifecycle
	if err != nil {
		t.Fatalf("websocket.Dial: %v", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "bye") }()

	codec, _ := protocol.NewFrameCodec()

	readDecoded := func() protocol.DecodedFrame {
		t.Helper()
		_, raw, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("conn.Read: %v", err)
		}
		f, err := codec.Decode(raw)
		if err != nil {
			t.Fatalf("codec.Decode: %v", err)
		}
		return f
	}

	firstFrame := readDecoded()
	if firstFrame.Type != protocol.FrameSnapshot {
		t.Fatalf("first frame type = 0x%02x, want FrameSnapshot (0x10)", firstFrame.Type)
	}
	if !strings.Contains(string(firstFrame.Data), "1:orbit") {
		t.Fatalf("snapshot did not contain rendered TUI view")
	}

	// Send unknown/corrupt frame first (ignored by server without dropping connection), then valid Ping.
	_ = conn.Write(ctx, websocket.MessageBinary, []byte{0xff, 0x00})
	// 3. Send FramePing (0x03) -> receive FramePong (0x12).
	pingBytes, _ := codec.EncodePing(1762268400123)
	if err := conn.Write(ctx, websocket.MessageBinary, pingBytes); err != nil {
		t.Fatalf("Write Ping: %v", err)
	}
	var gotPong bool
	for range 5 {
		f := readDecoded()
		if f.Type == protocol.FramePong && f.TimestampMS == 1762268400123 {
			gotPong = true
			break
		}
	}
	if !gotPong {
		t.Fatalf("did not receive matching FramePong")
	}

	// 4. Send FrameResize (0x02) -> triggers Snapshot/Diff and records activity.
	resizeBytes, _ := codec.EncodeResize(140, 42)
	if err := conn.Write(ctx, websocket.MessageBinary, resizeBytes); err != nil {
		t.Fatalf("Write Resize: %v", err)
	}
	var gotResizeFrame bool
	for range 5 {
		f := readDecoded()
		if f.Type == protocol.FrameSnapshot || f.Type == protocol.FrameDiff {
			gotResizeFrame = true
			break
		}
	}
	if !gotResizeFrame {
		t.Fatalf("expected Snapshot or Diff after Resize")
	}

	// 5. Send FrameInput (0x01) -> triggers dirty diff and records activity.
	inputBytes, _ := codec.EncodeInput([]byte{0x00}) // Ctrl+Space leader
	if err := conn.Write(ctx, websocket.MessageBinary, inputBytes); err != nil {
		t.Fatalf("Write Input: %v", err)
	}
	var gotLeaderDiff bool
	for range 5 {
		f := readDecoded()
		if (f.Type == protocol.FrameDiff || f.Type == protocol.FrameSnapshot) && strings.Contains(string(f.Data), "LEADER") {
			gotLeaderDiff = true
			break
		}
	}
	if !gotLeaderDiff {
		t.Fatalf("expected 60fps diff containing LEADER HUD")
	}
	if activityCount == 0 {
		t.Fatalf("expected OnActivity callback to be invoked on client input/resize")
	}

	// 6. BroadcastMetadata sends 0x13 FrameMetadata to connected client.
	if err := wsSrv.BroadcastMetadata(protocol.MetadataEvent{
		Type:        protocol.MetaEventIdleWarning,
		IdleSeconds: 1500,
		Message:     "Idle warning",
	}); err != nil {
		t.Fatalf("BroadcastMetadata: %v", err)
	}
	var gotMeta bool
	for range 5 {
		f := readDecoded()
		if f.Type == protocol.FrameMetadata && f.Metadata != nil && f.Metadata.Type == protocol.MetaEventIdleWarning {
			gotMeta = true
			break
		}
	}
	if !gotMeta {
		t.Fatalf("expected FrameMetadata event from BroadcastMetadata")
	}
}

func TestWSServer_MultiTabShellSpawnerContextAndSnapshot(t *testing.T) {
	t.Parallel()

	var (
		mu          sync.Mutex
		spawnedPanes = make(map[string]*pty.Pane)
		lastCtxReq  tui.ContextUpdateRequest
		snapCount   int
		failSpawn   bool
	)

	wsSrv, err := tui.NewWSServer(tui.WSServerConfig{
		ShellSpawner: func(_ context.Context, tabID string, cols, rows int) (*pty.Pane, error) {
			mu.Lock()
			defer mu.Unlock()
			if failSpawn {
				return nil, errors.New("pty spawn failed")
			}
			p := pty.NewVirtualPane(tabID, "Starship Shell "+tabID, cols, rows)
			p.FeedOutput([]byte("🪐 " + tabID + " $ "))
			spawnedPanes[tabID] = p
			return p, nil
		},
		OnActivity: func() {},
		OnContextUpdate: func(req tui.ContextUpdateRequest) error {
			mu.Lock()
			defer mu.Unlock()
			if req.OrbitURL == "bad" {
				return errors.New("context update failed")
			}
			lastCtxReq = req
			return nil
		},
		OnSnapshot: func(_ context.Context) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			snapCount++
			if snapCount > 1 {
				return nil, errors.New("snapshot failed")
			}
			return []string{"orbit"}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewWSServer: %v", err)
	}
	defer func() { _ = wsSrv.Close() }()

	httpSrv := httptest.NewServer(wsSrv.Handler())
	defer httpSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	codec, _ := protocol.NewFrameCodec()

	// 1. Connect to default tab (/ws) -> spawns tab-1 and sends initial scrollback snapshot.
	wsBase := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"
	conn1, _, err := websocket.Dial(ctx, wsBase, nil) //nolint:bodyclose
	if err != nil {
		t.Fatalf("Dial tab-1: %v", err)
	}
	defer func() { _ = conn1.CloseNow() }()

	_, rawSnap1, err := conn1.Read(ctx)
	if err != nil {
		t.Fatalf("Read tab-1 snap: %v", err)
	}
	f1, _ := codec.Decode(rawSnap1)
	if f1.Type != protocol.FrameSnapshot || !strings.Contains(string(f1.Data), "🪐 tab-1 $ ") {
		t.Fatalf("unexpected tab-1 snapshot: %+v (%q)", f1, string(f1.Data))
	}

	// 2. Connect to second tab (/ws?tab=tab-2) -> spawns independent tab-2 session.
	conn2, _, err := websocket.Dial(ctx, wsBase+"?tab=tab-2", nil) //nolint:bodyclose
	if err != nil {
		t.Fatalf("Dial tab-2: %v", err)
	}
	defer func() { _ = conn2.CloseNow() }()

	_, rawSnap2, err := conn2.Read(ctx)
	if err != nil {
		t.Fatalf("Read tab-2 snap: %v", err)
	}
	f2, _ := codec.Decode(rawSnap2)
	if f2.Type != protocol.FrameSnapshot || !strings.Contains(string(f2.Data), "🪐 tab-2 $ ") {
		t.Fatalf("unexpected tab-2 snapshot: %+v (%q)", f2, string(f2.Data))
	}

	// 3. Input, Resize, Ping, and raw output streaming on tab-1.
	inBytes, _ := codec.EncodeInput([]byte("orbit-helper status\n"))
	_ = conn1.Write(ctx, websocket.MessageBinary, inBytes)
	rszBytes, _ := codec.EncodeResize(132, 40)
	_ = conn1.Write(ctx, websocket.MessageBinary, rszBytes)
	pingBytes, _ := codec.EncodePing(42)
	_ = conn1.Write(ctx, websocket.MessageBinary, pingBytes)

	_, rawPong, err := conn1.Read(ctx)
	if err != nil {
		t.Fatalf("Read Pong: %v", err)
	}
	pongFrame, _ := codec.Decode(rawPong)
	if pongFrame.Type != protocol.FramePong || pongFrame.TimestampMS != 42 {
		t.Fatalf("unexpected Pong frame: %+v", pongFrame)
	}

	// Feed new output to tab-1 pane -> verify 0x11 FrameDiff arrives immediately on conn1.
	mu.Lock()
	p1 := spawnedPanes["tab-1"]
	mu.Unlock()
	p1.FeedOutput([]byte("workspace: orbit\r\n"))

	_, rawDiff, err := conn1.Read(ctx)
	if err != nil {
		t.Fatalf("Read Diff: %v", err)
	}
	diffFrame, _ := codec.Decode(rawDiff)
	if diffFrame.Type != protocol.FrameDiff || string(diffFrame.Data) != "workspace: orbit\r\n" {
		t.Fatalf("unexpected Diff frame: %+v (%q)", diffFrame, string(diffFrame.Data))
	}

	// 4. Reconnect to tab-1 -> receives combined scrollback in initial snapshot, then close with 4000 ("close-tab").
	conn1Reconnect, _, err := websocket.Dial(ctx, wsBase+"?tab=tab-1", nil) //nolint:bodyclose
	if err != nil {
		t.Fatalf("Dial tab-1 reconnect: %v", err)
	}
	_, rawReconSnap, err := conn1Reconnect.Read(ctx)
	if err != nil {
		t.Fatalf("Read reconnect snap: %v", err)
	}
	reconSnap, _ := codec.Decode(rawReconSnap)
	if !strings.Contains(string(reconSnap.Data), "workspace: orbit") {
		t.Fatalf("expected reconnect scrollback to include 'workspace: orbit', got %q", string(reconSnap.Data))
	}
	_ = conn1Reconnect.Close(4000, "close-tab")

	// 5. Close tab-2 via DELETE /api/v1/tabs/tab-2 and via CloseTab("").
	wsSrv.CloseTab("")
	delReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, httpSrv.URL+"/api/v1/tabs/tab-2", nil)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE tab-2: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE tab-2 status = %d, want 200", delResp.StatusCode)
	}

	// 6. POST /api/v1/context and POST /api/v1/snapshot endpoints.
	ctxPayload, _ := json.Marshal(tui.ContextUpdateRequest{
		OrbitURL:      "https://orbit.acme.dev",
		OrbitMCPToken: "omcp_tok_999",
		WorkspaceSlug: "acme",
	})
	ctxResp, err := http.Post(httpSrv.URL+"/api/v1/context", "application/json", bytes.NewReader(ctxPayload))
	if err != nil {
		t.Fatalf("POST /api/v1/context: %v", err)
	}
	_ = ctxResp.Body.Close()
	if ctxResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/context status = %d", ctxResp.StatusCode)
	}
	mu.Lock()
	gotOrbitURL := lastCtxReq.OrbitURL
	mu.Unlock()
	if gotOrbitURL != "https://orbit.acme.dev" {
		t.Fatalf("lastCtxReq.OrbitURL = %q, want https://orbit.acme.dev", gotOrbitURL)
	}

	// Bad JSON and error branch on /api/v1/context
	badJSONResp, _ := http.Post(httpSrv.URL+"/api/v1/context", "application/json", strings.NewReader("{bad"))
	_ = badJSONResp.Body.Close()
	if badJSONResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on bad JSON, got %d", badJSONResp.StatusCode)
	}
	errCtxPayload, _ := json.Marshal(tui.ContextUpdateRequest{OrbitURL: "bad"})
	errCtxResp, _ := http.Post(httpSrv.URL+"/api/v1/context", "application/json", bytes.NewReader(errCtxPayload))
	_ = errCtxResp.Body.Close()
	if errCtxResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 on context update error, got %d", errCtxResp.StatusCode)
	}

	// POST /api/v1/snapshot success and error branch
	snapResp, err := http.Post(httpSrv.URL+"/api/v1/snapshot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/v1/snapshot: %v", err)
	}
	_ = snapResp.Body.Close()
	if snapResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/snapshot status = %d", snapResp.StatusCode)
	}
	snapErrResp, _ := http.Post(httpSrv.URL+"/api/v1/snapshot", "application/json", nil)
	_ = snapErrResp.Body.Close()
	if snapErrResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 on second snapshot, got %d", snapErrResp.StatusCode)
	}

	// Spawner error closes connection cleanly
	mu.Lock()
	failSpawn = true
	mu.Unlock()
	connFail, _, err := websocket.Dial(ctx, wsBase+"?tab=tab-fail", nil) //nolint:bodyclose
	if err == nil {
		_, _, _ = connFail.Read(ctx)
		_ = connFail.CloseNow()
	}
}
