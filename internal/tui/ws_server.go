package tui

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
)

// WSServerConfig configures the in-Pod WebSocket server (:8080/ws).
type WSServerConfig struct {
	Compositor   *Compositor
	TickInterval time.Duration // Defaults to 16ms (~60fps)
	OnActivity   func()
}

// WSServer streams zstd-compressed 60fps TUI frames over binary WebSockets.
type WSServer struct {
	comp         *Compositor
	codec        *protocol.FrameCodec
	tickInterval time.Duration
	onActivity   func()

	mu      sync.Mutex
	clients map[*websocket.Conn]struct{}
}

// NewWSServer validates cfg and constructs a WSServer.
func NewWSServer(cfg WSServerConfig) (*WSServer, error) {
	if cfg.Compositor == nil {
		return nil, fmt.Errorf("%w: Compositor is required", domain.ErrInvalidConfig)
	}
	codec, _ := protocol.NewFrameCodec()
	tick := cfg.TickInterval
	if tick <= 0 {
		tick = 16 * time.Millisecond
	}
	return &WSServer{
		comp:         cfg.Compositor,
		codec:        codec,
		tickInterval: tick,
		onActivity:   cfg.OnActivity,
		clients:      make(map[*websocket.Conn]struct{}),
	}, nil
}

// Handler returns the HTTP handler serving GET /healthz and GET /ws.
func (s *WSServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /ws", s.handleWS)
	return mux
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
