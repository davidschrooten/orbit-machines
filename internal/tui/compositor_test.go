package tui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
