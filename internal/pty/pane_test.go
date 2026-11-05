package pty_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/pty"
)

func TestPane_RealPTYSpawnReadWriteResizeAndClose(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := pty.Spawn(ctx, pty.PaneConfig{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty PaneConfig, got %v", err)
	}
	if _, err := pty.Spawn(ctx, pty.PaneConfig{ID: "p1"}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for missing Command, got %v", err)
	}

	pane, err := pty.Spawn(ctx, pty.PaneConfig{
		ID:      "pane-shell",
		Title:   "Shell",
		Command: []string{"/bin/sh"},
		Cols:    80,
		Rows:    24,
	})
	if err != nil {
		t.Fatalf("Spawn(/bin/sh): %v", err)
	}
	defer func() { _ = pane.Close() }()

	if pane.ID() != "pane-shell" || pane.Title() != "Shell" {
		t.Fatalf("unexpected ID/Title: %s / %s", pane.ID(), pane.Title())
	}
	cols, rows := pane.Size()
	if cols != 80 || rows != 24 {
		t.Fatalf("Size() = (%d, %d), want (80, 24)", cols, rows)
	}

	// Send a command with ANSI color codes into the PTY and wait for VT emulator to render it.
	if _, err := pane.WriteInput([]byte("printf '\\033[32mORBIT_PTY_OK\\033[0m\\n'\n")); err != nil {
		t.Fatalf("WriteInput: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		if strings.Contains(pane.PlainText(), "ORBIT_PTY_OK") {
			found = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !found {
		t.Fatalf("expected VT screen to contain ORBIT_PTY_OK, got:\n%s", pane.PlainText())
	}
	if !strings.Contains(pane.Render(), "ORBIT_PTY_OK") {
		t.Fatalf("expected ANSI Render() to contain ORBIT_PTY_OK")
	}

	// Resize updates both VT emulator and PTY winsize and marks pane dirty.
	_ = pane.ConsumeDirty()
	if err := pane.Resize(100, 30); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if !pane.ConsumeDirty() {
		t.Fatalf("expected Resize to mark pane dirty")
	}
	cols, rows = pane.Size()
	if cols != 100 || rows != 30 {
		t.Fatalf("Size after Resize = (%d, %d), want (100, 30)", cols, rows)
	}

	// Ask shell to exit cleanly and verify Exited() transitions to true.
	_, _ = pane.WriteInput([]byte("exit 0\n"))
	for time.Now().Before(deadline) && !pane.Exited() {
		time.Sleep(15 * time.Millisecond)
	}
	if !pane.Exited() {
		t.Fatalf("expected pane.Exited() == true after exit 0")
	}
	if err := pane.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Second Close is idempotent.
	if err := pane.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	// Spawn with custom Dir, Env, default Title, and Close while process is still running.
	runningPane, err := pty.Spawn(ctx, pty.PaneConfig{
		ID:      "pane-sleep",
		Command: []string{"/bin/sh", "-c", "sleep 30"},
		Dir:     t.TempDir(),
		Env:     []string{"ORBIT_TEST_VAR=1"},
	})
	if err != nil {
		t.Fatalf("Spawn sleep: %v", err)
	}
	if runningPane.Title() != "/bin/sh" {
		t.Fatalf("expected default Title '/bin/sh', got %q", runningPane.Title())
	}
	if err := runningPane.Close(); err != nil {
		t.Fatalf("Close running pane: %v", err)
	}
}

func TestVirtualPane_DeterministicEmulationAndDirtyTracking(t *testing.T) {
	t.Parallel()

	vp := pty.NewVirtualPane("vp-1", "Claude Code", 0, 0)
	defer func() { _ = vp.Close() }()

	c, r := vp.Size()
	if c != 80 || r != 24 {
		t.Fatalf("expected default size 80x24, got %dx%d", c, r)
	}
	if !vp.ConsumeDirty() {
		t.Fatalf("expected newly created VirtualPane to be initially dirty")
	}
	if vp.ConsumeDirty() {
		t.Fatalf("expected second ConsumeDirty without output to return false")
	}

	vp.FeedOutput([]byte("\x1b[1;36mClaude Code Ready\x1b[0m\r\n"))
	if !vp.ConsumeDirty() {
		t.Fatalf("expected FeedOutput to mark pane dirty")
	}
	if !strings.Contains(vp.PlainText(), "Claude Code Ready") {
		t.Fatalf("expected PlainText to contain 'Claude Code Ready', got %q", vp.PlainText())
	}

	_, _ = vp.WriteInput([]byte("help\n"))
	if got := string(vp.RecordedInput()); got != "help\n" {
		t.Fatalf("RecordedInput = %q, want %q", got, "help\n")
	}

	if err := vp.Resize(120, 40); err != nil {
		t.Fatalf("Resize virtual pane: %v", err)
	}
	if err := vp.Resize(0, -1); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig on non-positive Resize, got %v", err)
	}

	// Scrollback and SetOnOutput streaming
	var streamed []byte
	vp.SetOnOutput(func(chunk []byte) {
		streamed = append(streamed, chunk...)
	})
	vp.FeedOutput(nil) // empty chunk is no-op
	vp.FeedOutput([]byte("stream-test-123"))
	if string(streamed) != "stream-test-123" {
		t.Fatalf("streamed = %q, want stream-test-123", string(streamed))
	}
	if !strings.Contains(string(vp.Scrollback()), "stream-test-123") {
		t.Fatalf("Scrollback missing stream-test-123")
	}

	// Verify scrollback ring buffer caps at MaxScrollbackBytes
	bigChunk := make([]byte, pty.MaxScrollbackBytes+1024)
	for i := range bigChunk {
		bigChunk[i] = 'Z'
	}
	vp.FeedOutput(bigChunk)
	if len(vp.Scrollback()) != pty.MaxScrollbackBytes {
		t.Fatalf("Scrollback len = %d, want %d", len(vp.Scrollback()), pty.MaxScrollbackBytes)
	}
}

func TestPane_RawPassthroughTerminalQueriesAndSIGWINCH(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pane, err := pty.Spawn(ctx, pty.PaneConfig{
		ID:             "pane-raw-tui",
		Title:          "Claude/Nvim TUI",
		Command:        []string{"/bin/sh"},
		Cols:           120,
		Rows:           36,
		RawPassthrough: true,
	})
	if err != nil {
		t.Fatalf("Spawn(/bin/sh, RawPassthrough): %v", err)
	}
	defer func() { _ = pane.Close() }()

	if !pane.RawPassthrough() {
		t.Fatalf("expected RawPassthrough() == true")
	}

	// Simulate full-screen TUI terminal queries (DSR \x1b[6n, DA1 \x1b[c, DA2 \x1b[>c,
	// OSC 10/11 color queries, DECRQM, Kitty keyboard query, alt-screen \x1b[?1049h)
	// that previously blocked charmbracelet/x/vt's unbuffered response pipe.
	for range 64 {
		pane.FeedOutput([]byte("\x1b[?1049h\x1b[6n\x1b[c\x1b[>c\x1b]10;?\x07\x1b]11;?\x07\x1b[?2026$p\x1b[?uTUI_FRAME_OK\r\n"))
	}
	if !strings.Contains(string(pane.Scrollback()), "TUI_FRAME_OK") {
		t.Fatalf("expected Scrollback to contain TUI_FRAME_OK")
	}

	// Install a SIGWINCH trap inside the PTY shell and verify both Resize (even with identical dimensions)
	// and Redraw/NotifyWinch deliver SIGWINCH to the child process.
	_, err = pane.WriteInput([]byte("trap 'printf \"WINCH_SIGNAL_OK\\n\"' WINCH; printf \"TRAP_READY\\n\"\n"))
	if err != nil {
		t.Fatalf("WriteInput trap: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(string(pane.Scrollback()), "TRAP_READY") {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	if err := pane.Resize(120, 36); err != nil {
		t.Fatalf("Resize(120, 36): %v", err)
	}
	pane.Redraw()

	var gotWinch bool
	for time.Now().Before(deadline) {
		if strings.Contains(string(pane.Scrollback()), "WINCH_SIGNAL_OK") {
			gotWinch = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !gotWinch {
		t.Fatalf("expected child process to receive SIGWINCH on Resize/Redraw, scrollback:\n%s", string(pane.Scrollback()))
	}
}
