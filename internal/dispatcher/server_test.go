package dispatcher_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/davidschrooten/orbit-machines/internal/dispatcher"
	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/k8s"
	"github.com/davidschrooten/orbit-machines/internal/orbitclient"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
	"github.com/davidschrooten/orbit-machines/internal/ticket"
)

const (
	testClusterToken = "omc_live_0123456789abcdef0123456789abcdef"
	testIngressHost  = "machines.acme.dev"
)

func newTestPodWSServer(t *testing.T, frameCodec *protocol.FrameCodec) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		ctx := r.Context()
		// Send initial 0x10 snapshot upon connection.
		snap, _ := frameCodec.EncodeSnapshot([]byte("\x1b[2Jorbit-machined ready $ "))
		_ = conn.Write(ctx, websocket.MessageBinary, snap)

		for {
			msgType, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if msgType != websocket.MessageBinary {
				continue
			}
			decoded, err := frameCodec.Decode(data)
			if err != nil {
				return
			}
			switch decoded.Type {
			case protocol.FrameInput:
				diff, _ := frameCodec.EncodeDiff(append([]byte("echo: "), decoded.Data...))
				_ = conn.Write(ctx, websocket.MessageBinary, diff)
			case protocol.FramePing:
				pong, _ := frameCodec.EncodePong(decoded.TimestampMS)
				_ = conn.Write(ctx, websocket.MessageBinary, pong)
			}
		}
	}))
}

func TestServer_WebSocketTerminalBootAndSplice(t *testing.T) {
	t.Parallel()

	frameCodec, err := protocol.NewFrameCodec()
	if err != nil {
		t.Fatalf("NewFrameCodec: %v", err)
	}
	podSrv := newTestPodWSServer(t, frameCodec)
	defer podSrv.Close()

	podWSURL := "ws" + strings.TrimPrefix(podSrv.URL, "http") + "/ws"
	fakeK8s := k8s.NewFakePodOrchestrator(10)
	fakeK8s.SetDefaultWebSocketURL(podWSURL)
	fakeOrbit := orbitclient.NewFakeClient()

	now := time.Now().UTC().Truncate(time.Second)
	ticketCodec, err := ticket.NewCodecFromClusterToken(testClusterToken)
	if err != nil {
		t.Fatalf("NewCodecFromClusterToken: %v", err)
	}

	srv, err := dispatcher.NewServer(dispatcher.ServerConfig{
		ClusterToken:      testClusterToken,
		MaxConcurrentPods: 10,
		TicketCodec:       ticketCodec,
		FrameCodec:        frameCodec,
		Orchestrator:      fakeK8s,
		OrbitClient:       fakeOrbit,
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	gwSrv := httptest.NewServer(srv.Handler())
	defer gwSrv.Close()

	// 1. Healthz endpoint returns 200 OK.
	resp, err := http.Get(gwSrv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// 2. Missing or invalid ticket returns 401 before WebSocket upgrade.
	unauthResp, err := http.Get(gwSrv.URL + "/ws/terminal?ticket=invalid")
	if err != nil {
		t.Fatalf("GET /ws/terminal invalid: %v", err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid ticket status = %d, want 401", unauthResp.StatusCode)
	}

	// 3. Mint valid ticket and connect via WebSocket.
	signedTicket, err := ticketCodec.Sign(ticket.Claims{
		JTI:           "jti_01HXYZ",
		Issuer:        ticket.ExpectedIssuer,
		Subject:       "usr_01HABC",
		WorkspaceID:   "ws_01HXYZ",
		WorkspaceSlug: "acme",
		UserLogin:     "alice",
		Tier:          domain.TierPerformance,
		IssuedAt:      now.Unix(),
		ExpiresAt:     now.Add(60 * time.Second).Unix(),
	})
	if err != nil {
		t.Fatalf("Sign ticket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := "ws" + strings.TrimPrefix(gwSrv.URL, "http") + "/ws/terminal?ticket=" + signedTicket
	clientConn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("websocket.Dial gateway: %v", err)
	}
	defer func() { _ = clientConn.CloseNow() }()

	// Read 0x13 boot progress metadata frames followed by 0x10 initial snapshot from Pod.
	var bootPhases []domain.BootPhase
	var initialSnapshot []byte
	for initialSnapshot == nil {
		_, raw, err := clientConn.Read(ctx)
		if err != nil {
			t.Fatalf("reading boot/snapshot frames: %v", err)
		}
		decoded, err := frameCodec.Decode(raw)
		if err != nil {
			t.Fatalf("Decode frame: %v", err)
		}
		switch decoded.Type {
		case protocol.FrameMetadata:
			if decoded.Metadata != nil && decoded.Metadata.Type == protocol.MetaEventBootProgress {
				bootPhases = append(bootPhases, decoded.Metadata.Phase)
			}
		case protocol.FrameSnapshot:
			initialSnapshot = decoded.Data
		}
	}

	if len(bootPhases) == 0 || bootPhases[len(bootPhases)-1] != domain.PhaseReady {
		t.Fatalf("expected boot progress ending in PhaseReady, got %v", bootPhases)
	}
	if !bytes.Contains(initialSnapshot, []byte("orbit-machined ready")) {
		t.Fatalf("unexpected initial snapshot: %q", initialSnapshot)
	}

	// Send 0x01 FrameInput and receive 0x11 FrameDiff spliced from the Pod.
	inputFrame, _ := frameCodec.EncodeInput([]byte("ls -la\n"))
	if err := clientConn.Write(ctx, websocket.MessageBinary, inputFrame); err != nil {
		t.Fatalf("Write FrameInput: %v", err)
	}

	_, diffRaw, err := clientConn.Read(ctx)
	if err != nil {
		t.Fatalf("Read FrameDiff: %v", err)
	}
	diffDecoded, err := frameCodec.Decode(diffRaw)
	if err != nil {
		t.Fatalf("Decode FrameDiff: %v", err)
	}
	if diffDecoded.Type != protocol.FrameDiff || string(diffDecoded.Data) != "echo: ls -la\n" {
		t.Fatalf("unexpected spliced diff frame: %+v", diffDecoded)
	}

	// 4. DELETE /api/v1/sessions/{ws_id}/{sub} terminates the Pod and closes active WebSocket.
	delReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, gwSrv.URL+"/api/v1/sessions/ws_01HXYZ/usr_01HABC", nil)
	delReq.Header.Set("Authorization", "Bearer "+testClusterToken)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE session: %v", err)
	}
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE session status = %d, want 200", delResp.StatusCode)
	}

	activePods, _ := fakeK8s.ListActivePods(ctx)
	if len(activePods) != 0 {
		t.Fatalf("expected 0 active pods after DELETE session, got %d", len(activePods))
	}
}

func TestServer_HeartbeatLoopAndRevocation(t *testing.T) {
	t.Parallel()

	fakeK8s := k8s.NewFakePodOrchestrator(10)
	fakeOrbit := orbitclient.NewFakeClient()
	ticketCodec, _ := ticket.NewCodecFromClusterToken(testClusterToken)
	frameCodec, _ := protocol.NewFrameCodec()

	srv, err := dispatcher.NewServer(dispatcher.ServerConfig{
		ClusterToken:      testClusterToken,
		MaxConcurrentPods: 10,
		TicketCodec:       ticketCodec,
		FrameCodec:        frameCodec,
		Orchestrator:      fakeK8s,
		OrbitClient:       fakeOrbit,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx := context.Background()
	mid, _ := domain.NewMachineID("ws_01", "usr_revoke_me")
	if _, err := fakeK8s.EnsurePod(ctx, k8s.PodRequest{MachineID: mid, Tier: domain.TierStandard}); err != nil {
		t.Fatalf("EnsurePod: %v", err)
	}

	fakeOrbit.RevokeOnHeartbeat = []string{"usr_revoke_me"}

	hbCtx, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
	defer cancel()
	if err := srv.RunHeartbeatLoop(hbCtx, 15*time.Millisecond); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunHeartbeatLoop: %v", err)
	}

	if len(fakeOrbit.Heartbeats) == 0 {
		t.Fatalf("expected at least 1 heartbeat sent")
	}
	active, _ := fakeK8s.ListActivePods(ctx)
	if len(active) != 0 {
		t.Fatalf("expected revoked session pod to be deleted, still have %+v", active)
	}
}

func TestServer_ValidationAndAuthErrors(t *testing.T) {
	t.Parallel()

	if _, err := dispatcher.NewServer(dispatcher.ServerConfig{}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty ServerConfig, got %v", err)
	}

	fakeK8s := k8s.NewFakePodOrchestrator(1)
	fakeOrbit := orbitclient.NewFakeClient()
	ticketCodec, _ := ticket.NewCodecFromClusterToken(testClusterToken)
	frameCodec, _ := protocol.NewFrameCodec()

	srv, err := dispatcher.NewServer(dispatcher.ServerConfig{
		ClusterToken:      testClusterToken,
		MaxConcurrentPods: 1,
		TicketCodec:       ticketCodec,
		FrameCodec:        frameCodec,
		Orchestrator:      fakeK8s,
		OrbitClient:       fakeOrbit,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	gwSrv := httptest.NewServer(srv.Handler())
	defer gwSrv.Close()

	// Unauthorized DELETE session
	req, _ := http.NewRequest(http.MethodDelete, gwSrv.URL+"/api/v1/sessions/ws_01/usr_01", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do unauthorized DELETE: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthorized DELETE, got %d", resp.StatusCode)
	}

	// Invalid machine ID in DELETE session
	badReq, _ := http.NewRequest(http.MethodDelete, gwSrv.URL+"/api/v1/sessions/ws..bad/usr_01", nil)
	badReq.Header.Set("Authorization", "Bearer "+testClusterToken)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("Do bad DELETE: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad DELETE path, got %d", badResp.StatusCode)
	}

	// Verify JSON body on valid DELETE session
	okReq, _ := http.NewRequest(http.MethodDelete, gwSrv.URL+"/api/v1/sessions/ws_01/usr_01", nil)
	okReq.Header.Set("Authorization", "Bearer "+testClusterToken)
	okResp, err := http.DefaultClient.Do(okReq)
	if err != nil {
		t.Fatalf("Do valid DELETE: %v", err)
	}
	defer func() { _ = okResp.Body.Close() }()
	var body map[string]string
	_ = json.NewDecoder(okResp.Body).Decode(&body)
	if body["status"] != "terminated" {
		t.Fatalf("unexpected DELETE response body: %+v", body)
	}
}

func TestServer_WebSocketEdgeCases(t *testing.T) {
	t.Parallel()

	ticketCodec, _ := ticket.NewCodecFromClusterToken(testClusterToken)
	frameCodec, _ := protocol.NewFrameCodec()
	fakeK8s := k8s.NewFakePodOrchestrator(1)
	// Point to an unreachable port so websocket.Dial to the Pod fails cleanly.
	fakeK8s.SetDefaultWebSocketURL("ws://127.0.0.1:1/ws")

	if _, err := dispatcher.NewServer(dispatcher.ServerConfig{ClusterToken: testClusterToken}); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig when dependencies are nil, got %v", err)
	}

	srv, err := dispatcher.NewServer(dispatcher.ServerConfig{
		ClusterToken: testClusterToken,
		TicketCodec:  ticketCodec,
		FrameCodec:   frameCodec,
		Orchestrator: fakeK8s,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	// SendHeartbeatOnce with nil OrbitClient is a no-op.
	if err := srv.SendHeartbeatOnce(context.Background()); err != nil {
		t.Fatalf("SendHeartbeatOnce with nil OrbitClient: %v", err)
	}

	gwSrv := httptest.NewServer(srv.Handler())
	defer gwSrv.Close()

	now := time.Now().UTC()
	t1, _ := ticketCodec.Sign(ticket.Claims{
		JTI:           "jti_1",
		Issuer:        ticket.ExpectedIssuer,
		Subject:       "usr_1",
		WorkspaceID:   "ws_1",
		WorkspaceSlug: "acme",
		UserLogin:     "alice",
		Tier:          domain.TierStandard,
		IssuedAt:      now.Unix(),
		ExpiresAt:     now.Add(60 * time.Second).Unix(),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	wsURL1 := "ws" + strings.TrimPrefix(gwSrv.URL, "http") + "/ws/terminal?ticket=" + t1
	conn1, _, err := websocket.Dial(ctx, wsURL1, nil)
	if err != nil {
		t.Fatalf("Dial 1: %v", err)
	}
	defer func() { _ = conn1.CloseNow() }()

	// Drain until the connection closes with StatusBadGateway because Pod WS is unreachable.
	for {
		if _, _, err := conn1.Read(ctx); err != nil {
			break
		}
	}

	// Now capacity (1) is full with usr_1's pod; connecting as usr_2 hits ErrClusterCapacityReached.
	t2, _ := ticketCodec.Sign(ticket.Claims{
		JTI:           "jti_2",
		Issuer:        ticket.ExpectedIssuer,
		Subject:       "usr_2",
		WorkspaceID:   "ws_1",
		WorkspaceSlug: "acme",
		UserLogin:     "bob",
		Tier:          domain.TierStandard,
		IssuedAt:      now.Unix(),
		ExpiresAt:     now.Add(60 * time.Second).Unix(),
	})
	wsURL2 := "ws" + strings.TrimPrefix(gwSrv.URL, "http") + "/ws/terminal?ticket=" + t2
	conn2, _, err := websocket.Dial(ctx, wsURL2, nil)
	if err != nil {
		t.Fatalf("Dial 2: %v", err)
	}
	defer func() { _ = conn2.CloseNow() }()
	if _, _, err := conn2.Read(ctx); err == nil {
		t.Fatalf("expected connection 2 to close when cluster capacity is reached")
	}
}

