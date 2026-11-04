// Package tui implements the in-Pod 60fps Bubble Tea tiling window manager,
// browser-safe leader key engine (Ctrl+Space), mouse split resizer, and
// binary WebSocket frame server for orbit-machined.
package tui

import (
	"fmt"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/davidschrooten/orbit-machines/internal/pty"
)

// SplitLayout describes how panes in a repository tab are tiled.
type SplitLayout string

const (
	LayoutVertical   SplitLayout = "vertical"
	LayoutHorizontal SplitLayout = "horizontal"
)

// ModalKind identifies the currently active modal overlay in the compositor.
type ModalKind string

const (
	ModalNone           ModalKind = ""
	ModalToolLauncher   ModalKind = "tool_launcher"
	ModalRepoSwitcher   ModalKind = "repo_switcher"
	ModalIssuePicker    ModalKind = "issue_picker"
	ModalAIConfigurator ModalKind = "ai_configurator"
	ModalFlakeWizard    ModalKind = "flake_wizard"
	ModalWIPRestore     ModalKind = "wip_restore"
)

// RepoTabConfig configures a repository tab in the workspace TUI.
type RepoTabConfig struct {
	Name     string
	Path     string
	HasFlake bool
}

// PaneSpawner creates a new PTY/VT pane with the given ID, title, and dimensions.
type PaneSpawner func(id, title string, cols, rows int) *pty.Pane

// CompositorConfig configures the root Bubble Tea Compositor.
type CompositorConfig struct {
	WorkspaceSlug string
	ResourceTier  string
	LeaderByte    byte // Defaults to 0x00 (Ctrl+Space / Ctrl+@)
	Repositories  []RepoTabConfig
	SpawnPane     PaneSpawner
}

type repoTab struct {
	cfg        RepoTabConfig
	panes      []*pty.Pane
	focusedIdx int
	layout     SplitLayout
	splitRatio float64
	zoomed     bool
}

// Compositor is the thread-safe Bubble Tea root model and tiling window manager.
type Compositor struct {
	mu            sync.Mutex
	workspaceSlug string
	resourceTier  string
	leaderByte    byte
	leaderPending bool
	width         int
	height        int
	tabs          []*repoTab
	activeTab     int
	activeModal   ModalKind
	modalViews    map[ModalKind]func(width, height int) string
	spawner       PaneSpawner
	paneSeq       int
	uiDirty       bool
}

// NewCompositor initializes the tiling window manager with its repository tabs and initial pane.
func NewCompositor(cfg CompositorConfig) *Compositor {
	spawner := cfg.SpawnPane
	if spawner == nil {
		spawner = pty.NewVirtualPane
	}
	repos := cfg.Repositories
	if len(repos) == 0 {
		repos = []RepoTabConfig{{Name: "workspace", HasFlake: true}}
	}
	wsSlug := strings.TrimSpace(cfg.WorkspaceSlug)
	if wsSlug == "" {
		wsSlug = "orbit"
	}
	tier := strings.TrimSpace(cfg.ResourceTier)
	if tier == "" {
		tier = "standard"
	}

	c := &Compositor{
		workspaceSlug: wsSlug,
		resourceTier:  tier,
		leaderByte:    cfg.LeaderByte, // 0x00 is Ctrl+Space
		width:         120,
		height:        36,
		modalViews:    make(map[ModalKind]func(width, height int) string),
		spawner:       spawner,
		uiDirty:       true,
	}

	for _, r := range repos {
		tab := &repoTab{
			cfg:        r,
			layout:     LayoutVertical,
			splitRatio: 0.5,
		}
		c.tabs = append(c.tabs, tab)
	}
	c.ensureTabInitializedLocked(0)
	return c
}

// Init implements tea.Model.
func (c *Compositor) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (c *Compositor) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		c.Resize(m.Width, m.Height)
	case tea.KeyMsg:
		c.HandleRawInput([]byte(m.String()))
	}
	return c, nil
}

// Resize updates the terminal dimensions and recalculates all visible pane PTY sizes.
func (c *Compositor) Resize(cols, rows int) {
	if cols < 20 {
		cols = 20
	}
	if rows < 6 {
		rows = 6
	}
	c.mu.Lock()
	c.width = cols
	c.height = rows
	c.recomputePaneSizesLocked()
	c.uiDirty = true
	c.mu.Unlock()
}

// Size returns the current terminal width and height.
func (c *Compositor) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.width, c.height
}

// HandleRawInput processes raw keyboard bytes through the Ctrl+Space leader engine,
// modal dispatcher, or active pane PTY.
func (c *Compositor) HandleRawInput(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i := 0; i < len(data); i++ {
		b := data[i]

		if c.leaderPending {
			c.leaderPending = false
			c.uiDirty = true
			c.executeLeaderChordLocked(b)
			continue
		}

		if b == c.leaderByte {
			c.leaderPending = true
			c.uiDirty = true
			continue
		}

		if c.activeModal != ModalNone {
			if b == 0x1b { // Escape dismisses modal
				c.activeModal = ModalNone
				c.uiDirty = true
			}
			continue
		}

		// Forward remaining slice directly to the focused pane if no leader byte is inside it.
		pane := c.focusedPaneLocked()
		if pane != nil {
			_, _ = pane.WriteInput([]byte{b})
		}
	}
}

func (c *Compositor) executeLeaderChordLocked(b byte) {
	tab := c.tabs[c.activeTab]
	switch b {
	case 'c':
		c.activeModal = ModalToolLauncher
	case 'r':
		c.activeModal = ModalRepoSwitcher
	case 'i':
		c.activeModal = ModalIssuePicker
	case 'a':
		c.activeModal = ModalAIConfigurator
	case 'v':
		tab.layout = LayoutVertical
		c.spawnPaneInTabLocked(tab, "Shell")
	case 's':
		tab.layout = LayoutHorizontal
		c.spawnPaneInTabLocked(tab, "Shell")
	case 'h', 'k':
		if len(tab.panes) > 0 {
			tab.focusedIdx = (tab.focusedIdx - 1 + len(tab.panes)) % len(tab.panes)
		}
	case 'l', 'j':
		if len(tab.panes) > 0 {
			tab.focusedIdx = (tab.focusedIdx + 1) % len(tab.panes)
		}
	case 'z':
		tab.zoomed = !tab.zoomed
		c.recomputePaneSizesLocked()
	case 'x':
		if len(tab.panes) > 1 {
			closed := tab.panes[tab.focusedIdx]
			_ = closed.Close()
			tab.panes = append(tab.panes[:tab.focusedIdx], tab.panes[tab.focusedIdx+1:]...)
			if tab.focusedIdx >= len(tab.panes) {
				tab.focusedIdx = len(tab.panes) - 1
			}
			c.recomputePaneSizesLocked()
		}
	default:
		if b >= '1' && b <= '9' {
			idx := int(b - '1')
			if idx < len(c.tabs) {
				c.activeTab = idx
				c.ensureTabInitializedLocked(idx)
			}
		}
	}
}

// DragSplitBorder adjusts the active tab's split ratio by delta, clamping to [0.2, 0.8].
func (c *Compositor) DragSplitBorder(delta float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tab := c.tabs[c.activeTab]
	ratio := tab.splitRatio + delta
	if ratio < 0.2 {
		ratio = 0.2
	}
	if ratio > 0.8 {
		ratio = 0.8
	}
	tab.splitRatio = ratio
	c.recomputePaneSizesLocked()
	c.uiDirty = true
}

// SetModalRenderer registers a custom view renderer for a ModalKind (e.g. FlakeWizard or AIConfigurator).
func (c *Compositor) SetModalRenderer(kind ModalKind, fn func(width, height int) string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.modalViews[kind] = fn
	c.uiDirty = true
}

// OpenModal opens a specific modal overlay.
func (c *Compositor) OpenModal(kind ModalKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeModal = kind
	c.uiDirty = true
}

// FocusedPane returns the currently focused pane in the active repository tab.
func (c *Compositor) FocusedPane() *pty.Pane {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.focusedPaneLocked()
}

// ActivePanes returns the panes in the active repository tab.
func (c *Compositor) ActivePanes() []*pty.Pane {
	c.mu.Lock()
	defer c.mu.Unlock()
	tab := c.tabs[c.activeTab]
	out := make([]*pty.Pane, len(tab.panes))
	copy(out, tab.panes)
	return out
}

// IsLeaderPending reports whether the compositor is waiting for a leader chord key.
func (c *Compositor) IsLeaderPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaderPending
}

// IsZoomed reports whether the active tab's focused pane is maximized.
func (c *Compositor) IsZoomed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tabs[c.activeTab].zoomed
}

// SplitLayout returns the active tab's split layout.
func (c *Compositor) SplitLayout() SplitLayout {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tabs[c.activeTab].layout
}

// SplitRatio returns the active tab's primary split ratio.
func (c *Compositor) SplitRatio() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tabs[c.activeTab].splitRatio
}

// ActiveModal returns the currently open modal overlay kind.
func (c *Compositor) ActiveModal() ModalKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeModal
}

// ActiveTabIndex returns the 0-based index of the active repository tab.
func (c *Compositor) ActiveTabIndex() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeTab
}

// ConsumeDirty returns true if either the compositor UI state or any active pane is dirty.
func (c *Compositor) ConsumeDirty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	dirty := c.uiDirty
	c.uiDirty = false
	for _, p := range c.tabs[c.activeTab].panes {
		if p.ConsumeDirty() {
			dirty = true
		}
	}
	return dirty
}

// View renders the full-screen TUI frame (top tab bar, tiled panes, modal overlay, and bottom HUD).
func (c *Compositor) View() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	topBar := c.renderTopBarLocked()
	bodyHeight := max(c.height-2, 4)
	body := c.renderBodyLocked(c.width, bodyHeight)
	hud := c.renderHUDLocked()

	return strings.Join([]string{topBar, body, hud}, "\n")
}

// Close closes all child PTY panes across all repository tabs.
func (c *Compositor) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tab := range c.tabs {
		for _, p := range tab.panes {
			_ = p.Close()
		}
	}
	return nil
}

func (c *Compositor) ensureTabInitializedLocked(idx int) {
	tab := c.tabs[idx]
	if !tab.cfg.HasFlake {
		c.activeModal = ModalFlakeWizard
	}
	if len(tab.panes) == 0 {
		c.spawnPaneInTabLocked(tab, "Claude Code")
	}
	c.recomputePaneSizesLocked()
}

func (c *Compositor) spawnPaneInTabLocked(tab *repoTab, title string) {
	c.paneSeq++
	id := fmt.Sprintf("pane-%d", c.paneSeq)
	p := c.spawner(id, title, max(c.width-2, 20), max(c.height-4, 6))
	tab.panes = append(tab.panes, p)
	tab.focusedIdx = len(tab.panes) - 1
	c.recomputePaneSizesLocked()
}

func (c *Compositor) focusedPaneLocked() *pty.Pane {
	tab := c.tabs[c.activeTab]
	if len(tab.panes) == 0 {
		return nil
	}
	return tab.panes[tab.focusedIdx]
}

func (c *Compositor) recomputePaneSizesLocked() {
	tab := c.tabs[c.activeTab]
	n := len(tab.panes)
	if n == 0 {
		return
	}
	innerW := max(c.width-2, 10)
	innerH := max(c.height-4, 4)

	if tab.zoomed || n == 1 {
		_ = tab.panes[tab.focusedIdx].Resize(innerW, innerH)
		return
	}

	if tab.layout == LayoutVertical {
		firstW := max(int(float64(innerW)*tab.splitRatio), 8)
		remW := max(innerW-firstW, 8)
		_ = tab.panes[0].Resize(firstW, innerH)
		eachW := max(remW/(n-1), 8)
		for i := 1; i < n; i++ {
			_ = tab.panes[i].Resize(eachW, innerH)
		}
		return
	}

	firstH := max(int(float64(innerH)*tab.splitRatio), 3)
	remH := max(innerH-firstH, 3)
	_ = tab.panes[0].Resize(innerW, firstH)
	eachH := max(remH/(n-1), 3)
	for i := 1; i < n; i++ {
		_ = tab.panes[i].Resize(innerW, eachH)
	}
}

func (c *Compositor) renderTopBarLocked() string {
	var tabs []string
	for i, t := range c.tabs {
		label := fmt.Sprintf(" %d:%s ", i+1, t.cfg.Name)
		if i == c.activeTab {
			tabs = append(tabs, "["+strings.TrimSpace(label)+"*]")
		} else {
			tabs = append(tabs, label)
		}
	}
	left := "ORBIT [" + c.workspaceSlug + "] " + strings.Join(tabs, "|")
	right := "[+ Claude] [+ Agy] [+ OpenCode] [+ Shell] [✦ Configure]"
	return left + "  " + right
}

func (c *Compositor) renderBodyLocked(width, height int) string {
	if c.activeModal != ModalNone {
		if custom, ok := c.modalViews[c.activeModal]; ok {
			return custom(width, height)
		}
		return c.renderDefaultModalLocked(width, height)
	}

	tab := c.tabs[c.activeTab]
	if len(tab.panes) == 0 {
		return ""
	}

	focusedStyle := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("63"))
	unfocusedStyle := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))

	renderOne := func(idx int, p *pty.Pane) string {
		header := fmt.Sprintf(" %s (%s) ", p.Title(), p.ID())
		content := header + "\n" + p.Render()
		if idx == tab.focusedIdx {
			return focusedStyle.Render(content)
		}
		return unfocusedStyle.Render(content)
	}

	if tab.zoomed || len(tab.panes) == 1 {
		return renderOne(tab.focusedIdx, tab.panes[tab.focusedIdx])
	}

	rendered := make([]string, 0, len(tab.panes))
	for i, p := range tab.panes {
		rendered = append(rendered, renderOne(i, p))
	}
	if tab.layout == LayoutVertical {
		return lipgloss.JoinHorizontal(lipgloss.Top, rendered...)
	}
	return lipgloss.JoinVertical(lipgloss.Left, rendered...)
}

func (c *Compositor) renderDefaultModalLocked(width, _ int) string {
	var title, body string
	switch c.activeModal {
	case ModalToolLauncher:
		title = "Tool Launcher"
		body = "1) Claude Code  2) Antigravity-CLI (agy)  3) OpenCode  4) Shell  [Esc to close]"
	case ModalRepoSwitcher:
		title = "Repository Switcher"
		body = "Select a workspace repository tab (1-9)  [Esc to close]"
	case ModalIssuePicker:
		title = "Orbit Issue Picker"
		body = "Checkout branch and inject Orbit issue context into active agent  [Esc to close]"
	case ModalAIConfigurator:
		title = "✦ Configure through an AI"
		body = "Describe your desired keybindings, layout, shell profile, or flake packages  [Esc to close]"
	case ModalFlakeWizard:
		title = "Flake Init Wizard"
		body = "No flake.nix detected in repository. Press Space to toggle Nixpkgs or Enter to generate  [Esc to close]"
	case ModalWIPRestore:
		title = "Restore Uncommitted WIP Bundle"
		body = "An uncommitted Git working tree snapshot was saved before idle shutdown. Restore now?  [Esc to close]"
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		Padding(1, 2).
		Width(min(max(width-8, 40), 90)).
		Render(title + "\n\n" + body)
	return box
}

func (c *Compositor) renderHUDLocked() string {
	tab := c.tabs[c.activeTab]
	paneTitle := "none"
	if fp := c.focusedPaneLocked(); fp != nil {
		paneTitle = fp.Title()
	}
	zoomBadge := ""
	if tab.zoomed {
		zoomBadge = " [ZOOM]"
	}
	if c.leaderPending {
		return "LEADER (Ctrl+Space) → c:Launch v:VSplit s:HSplit hjkl:Focus z:Zoom x:Close r:Repos i:Issues a:AI 1-9:Tab"
	}
	return fmt.Sprintf("Repo: %s | Pane: %s (%d/%d)%s | Layout: %s (%.0f%%) | Tier: %s | Leader: Ctrl+Space",
		tab.cfg.Name, paneTitle, tab.focusedIdx+1, len(tab.panes), zoomBadge, tab.layout, tab.splitRatio*100, c.resourceTier)
}
