// Package pty manages child pseudo-terminals (creack/pty) paired with
// thread-safe in-memory VT100/ANSI screen emulators (charmbracelet/x/vt).
package pty

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
	"github.com/davidschrooten/orbit-machines/internal/domain"
)

// MaxScrollbackBytes is the maximum number of raw output bytes retained per Pane
// for instant tab snapshot replay upon WebSocket reconnect.
const MaxScrollbackBytes = 64 * 1024

// PaneConfig configures a child PTY process and its virtual terminal buffer.
type PaneConfig struct {
	ID             string
	Title          string
	Command        []string
	Dir            string
	Env            []string
	Cols           int
	Rows           int
	RawPassthrough bool
	OnOutput       func(chunk []byte)
}

// Pane couples a child PTY (or headless virtual buffer) with a VT emulator.
type Pane struct {
	id             string
	title          string
	rawPassthrough bool

	mu         sync.Mutex
	emulator   *vt.Emulator
	cols       int
	rows       int
	ptmx       *os.File
	cmd        *exec.Cmd
	recInput   bytes.Buffer
	scrollback []byte
	onOutput   func(chunk []byte)
	closed     bool

	dirty  atomic.Bool
	exited atomic.Bool
}

// Spawn starts cfg.Command attached to a real pseudo-terminal and streams its
// stdout/stderr into an in-memory VT emulator (or directly to OnOutput when RawPassthrough is set).
func Spawn(ctx context.Context, cfg PaneConfig) (*Pane, error) {
	if strings.TrimSpace(cfg.ID) == "" {
		return nil, fmt.Errorf("%w: pane ID is required", domain.ErrInvalidConfig)
	}
	if len(cfg.Command) == 0 || strings.TrimSpace(cfg.Command[0]) == "" {
		return nil, fmt.Errorf("%w: pane Command is required", domain.ErrInvalidConfig)
	}
	cols, rows := normalizeDimensions(cfg.Cols, cfg.Rows)
	title := strings.TrimSpace(cfg.Title)
	if title == "" {
		title = cfg.Command[0]
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	if cfg.Dir != "" {
		cmd.Dir = cfg.Dir
	}
	if len(cfg.Env) > 0 {
		cmd.Env = cfg.Env
	}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return nil, fmt.Errorf("starting pty for %q: %w", cfg.Command[0], err)
	}

	em := vt.NewEmulator(cols, rows)
	p := &Pane{
		id:             cfg.ID,
		title:          title,
		rawPassthrough: cfg.RawPassthrough,
		emulator:       em,
		cols:           cols,
		rows:           rows,
		ptmx:           ptmx,
		cmd:            cmd,
		onOutput:       cfg.OnOutput,
	}
	p.dirty.Store(true)

	go p.readLoop()
	go p.waitLoop()
	return p, nil
}

// NewVirtualPane creates a deterministic in-memory VT pane without forking an OS process.
func NewVirtualPane(id, title string, cols, rows int) *Pane {
	c, r := normalizeDimensions(cols, rows)
	em := vt.NewEmulator(c, r)
	p := &Pane{
		id:       id,
		title:    title,
		emulator: em,
		cols:     c,
		rows:     r,
	}
	p.dirty.Store(true)
	return p
}

// ID returns the pane's unique identifier.
func (p *Pane) ID() string { return p.id }

// Title returns the pane's human-readable title.
func (p *Pane) Title() string { return p.title }

// RawPassthrough reports whether the pane bypasses in-memory VT emulation on output chunks.
func (p *Pane) RawPassthrough() bool { return p.rawPassthrough }

// Size returns the current terminal width (cols) and height (rows).
func (p *Pane) Size() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

// WriteInput sends raw keyboard/mouse bytes to the child PTY or records them on a virtual pane.
func (p *Pane) WriteInput(data []byte) (int, error) {
	p.mu.Lock()
	ptmx := p.ptmx
	if ptmx == nil {
		n, _ := p.recInput.Write(data)
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()
	return ptmx.Write(data)
}

// RecordedInput returns bytes written to a virtual pane via WriteInput.
func (p *Pane) RecordedInput() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]byte, p.recInput.Len())
	copy(out, p.recInput.Bytes())
	return out
}

// FeedOutput writes raw ANSI output directly into the VT emulator (when RawPassthrough is false),
// appends to scrollback, marks the pane dirty, and notifies any registered output listener.
func (p *Pane) FeedOutput(data []byte) {
	if len(data) == 0 {
		return
	}
	chunk := make([]byte, len(data))
	copy(chunk, data)

	p.mu.Lock()
	if !p.rawPassthrough && p.emulator != nil {
		_, _ = p.emulator.Write(chunk)
	}
	p.appendScrollbackLocked(chunk)
	cb := p.onOutput
	p.mu.Unlock()

	p.dirty.Store(true)
	if cb != nil {
		cb(chunk)
	}
}

// SetOnOutput registers a callback invoked with every raw output chunk produced by the pane.
func (p *Pane) SetOnOutput(fn func(chunk []byte)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.onOutput = fn
}

// Scrollback returns a copy of the most recent raw output bytes (up to MaxScrollbackBytes).
func (p *Pane) Scrollback() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]byte, len(p.scrollback))
	copy(out, p.scrollback)
	return out
}

func (p *Pane) appendScrollbackLocked(chunk []byte) {
	p.scrollback = append(p.scrollback, chunk...)
	if len(p.scrollback) > MaxScrollbackBytes {
		excess := len(p.scrollback) - MaxScrollbackBytes
		trimmed := make([]byte, MaxScrollbackBytes)
		copy(trimmed, p.scrollback[excess:])
		p.scrollback = trimmed
	}
}

// Resize updates both the VT emulator dimensions and the OS PTY window size (TIOCSWINSZ),
// and signals SIGWINCH to the child process group so full-screen TUI apps redraw.
func (p *Pane) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return fmt.Errorf("%w: pane dimensions must be positive (%dx%d)", domain.ErrInvalidConfig, cols, rows)
	}
	p.mu.Lock()
	p.cols = cols
	p.rows = rows
	if !p.rawPassthrough && p.emulator != nil {
		p.emulator.Resize(cols, rows)
	}
	ptmx := p.ptmx
	cmd := p.cmd
	p.mu.Unlock()

	p.dirty.Store(true)

	if ptmx != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{
			Cols: uint16(cols),
			Rows: uint16(rows),
		})
	}
	if cmd != nil && cmd.Process != nil && !p.exited.Load() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGWINCH)
		_ = cmd.Process.Signal(syscall.SIGWINCH)
	}
	return nil
}

// NotifyWinch re-applies the current PTY window size and sends SIGWINCH to the child process group.
func (p *Pane) NotifyWinch() {
	p.mu.Lock()
	cols, rows := p.cols, p.rows
	ptmx := p.ptmx
	cmd := p.cmd
	p.mu.Unlock()

	if ptmx != nil {
		_ = pty.Setsize(ptmx, &pty.Winsize{
			Cols: uint16(cols),
			Rows: uint16(rows),
		})
	}
	if cmd != nil && cmd.Process != nil && !p.exited.Load() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGWINCH)
		_ = cmd.Process.Signal(syscall.SIGWINCH)
	}
}

// Redraw triggers a terminal size refresh and SIGWINCH signal for full-screen TUI applications.
func (p *Pane) Redraw() {
	p.NotifyWinch()
}

// ConsumeDirty atomically clears and returns whether the pane has new state to render.
func (p *Pane) ConsumeDirty() bool {
	return p.dirty.Swap(false)
}

// Render returns the ANSI-formatted virtual screen contents.
func (p *Pane) Render() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emulator.Render()
}

// PlainText returns the plain-text virtual screen contents without ANSI escapes.
func (p *Pane) PlainText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.emulator.String()
}

// Exited reports whether the underlying child process has terminated.
func (p *Pane) Exited() bool {
	return p.exited.Load()
}

// Close terminates any running child process and releases PTY and VT resources.
func (p *Pane) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	ptmx := p.ptmx
	cmd := p.cmd
	_ = p.emulator.Close()
	p.mu.Unlock()

	if cmd != nil && cmd.Process != nil && !p.exited.Load() {
		_ = cmd.Process.Kill()
	}
	if ptmx != nil {
		_ = ptmx.Close()
	}
	return nil
}

func (p *Pane) readLoop() {
	buf := make([]byte, 16*1024)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			p.FeedOutput(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (p *Pane) waitLoop() {
	if p.cmd != nil {
		_ = p.cmd.Wait()
	}
	p.exited.Store(true)
	p.dirty.Store(true)
}

func normalizeDimensions(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	return cols, rows
}
