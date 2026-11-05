package dispatcher_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

	podWSURL := "ws" + strings.TrimPrefix(podSrv.URL, "http") + "/ws?existing=1"
	fakeK8s := k8s.NewFakePodOrchestrator(10)
	fakeK8s.SetDefaultWebSocketURL(podWSURL)
	fakeOrbit := orbitclient.NewFakeClient()
	fakeOrbit.FailSubject = "usr_01habc" // triggers fallback to claims.UserLogin ("alice")

	now := time.Now().UTC().Truncate(time.Second)
	ticketCodec, err := ticket.NewCodecFromClusterToken(testClusterToken)
	if err != nil {
		t.Fatalf("NewCodecFromClusterToken: %v", err)
	}

	srv, err := dispatcher.NewServer(dispatcher.ServerConfig{
		OrbitURL:          "https://orbit.acme.dev",
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

	wsURL := "ws" + strings.TrimPrefix(gwSrv.URL, "http") + "/ws/terminal?ticket=" + signedTicket + "&tab=tab-2"
	clientConn, _, err := websocket.Dial(ctx, wsURL, nil) //nolint:bodyclose // coder/websocket manages response body lifecycle
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

	// Reconnect a second client for the same MachineID (exercises registerActiveSession replacing previous session)
	// and close with 4000 ("close-tab") to exercise pumpWebSocket close-tab forwarding.
	clientConn2, _, err := websocket.Dial(ctx, wsURL, nil) //nolint:bodyclose
	if err == nil {
		for {
			_, raw2, rErr := clientConn2.Read(ctx)
			if rErr != nil {
				break
			}
			if dec2, dErr := frameCodec.Decode(raw2); dErr == nil && dec2.Type == protocol.FrameSnapshot {
				break
			}
		}
		_ = clientConn2.Close(4000, "close-tab")
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
	fakeOrbit.FailSubject = "usr_01habc" // triggers fallback to claims.UserLogin ("alice")
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
	fakeOrbit.FailSubject = "usr_01habc" // triggers fallback to claims.UserLogin ("alice")
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
	conn1, _, err := websocket.Dial(ctx, wsURL1, nil) //nolint:bodyclose // coder/websocket manages response body lifecycle
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
	conn2, _, err := websocket.Dial(ctx, wsURL2, nil) //nolint:bodyclose // coder/websocket manages response body lifecycle
	if err != nil {
		t.Fatalf("Dial 2: %v", err)
	}
	defer func() { _ = conn2.CloseNow() }()
	if _, _, err := conn2.Read(ctx); err == nil {
		t.Fatalf("expected connection 2 to close when cluster capacity is reached")
	}
}

func TestServer_MultiTabConcurrencyAndShutdownVariants(t *testing.T) {
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
		Now:               func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	gwSrv := httptest.NewServer(srv.Handler())
	defer gwSrv.Close()

	signedTicket, err := ticketCodec.Sign(ticket.Claims{
		JTI:           "jti_multitab",
		Issuer:        ticket.ExpectedIssuer,
		Subject:       "usr_multitab",
		WorkspaceID:   "ws_multitab",
		WorkspaceSlug: "acme",
		UserLogin:     "alice",
		Tier:          domain.TierStandard,
		IssuedAt:      now.Unix(),
		ExpiresAt:     now.Add(60 * time.Second).Unix(),
	})
	if err != nil {
		t.Fatalf("Sign ticket: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialAndDrainToSnapshot := func(tab string) *websocket.Conn {
		t.Helper()
		wsURL := "ws" + strings.TrimPrefix(gwSrv.URL, "http") + "/ws/terminal?ticket=" + signedTicket + "&tab=" + tab
		conn, _, dialErr := websocket.Dial(ctx, wsURL, nil) //nolint:bodyclose
		if dialErr != nil {
			t.Fatalf("Dial %s: %v", tab, dialErr)
		}
		for {
			_, raw, rErr := conn.Read(ctx)
			if rErr != nil {
				t.Fatalf("Read initial frames on %s: %v", tab, rErr)
			}
			if dec, dErr := frameCodec.Decode(raw); dErr == nil && dec.Type == protocol.FrameSnapshot {
				return conn
			}
		}
	}

	// 1. Open Tab 1 and Tab 2 concurrently for the same MachineID.
	// Verify Tab 2 connecting does NOT cancel Tab 1!
	tab1Conn := dialAndDrainToSnapshot("tab-1")
	defer func() { _ = tab1Conn.CloseNow() }()

	tab2Conn := dialAndDrainToSnapshot("tab-2")
	defer func() { _ = tab2Conn.CloseNow() }()

	for name, conn := range map[string]*websocket.Conn{"tab-1": tab1Conn, "tab-2": tab2Conn} {
		inFrame, _ := frameCodec.EncodeInput([]byte("hello-" + name + "\n"))
		if err := conn.Write(ctx, websocket.MessageBinary, inFrame); err != nil {
			t.Fatalf("Write on %s while other tab open: %v", name, err)
		}
		_, rawDiff, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read diff on %s while other tab open: %v", name, err)
		}
		dec, err := frameCodec.Decode(rawDiff)
		if err != nil || dec.Type != protocol.FrameDiff || string(dec.Data) != "echo: hello-"+name+"\n" {
			t.Fatalf("unexpected diff on %s: %+v, err=%v", name, dec, err)
		}
	}

	// 2. Closing Tab 2 with close status 4001 ("shutdown-machine") triggers Pod deletion and closes Tab 1.
	_ = tab2Conn.Close(4001, "shutdown-machine")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		active, _ := fakeK8s.ListActivePods(ctx)
		if len(active) == 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if active, _ := fakeK8s.ListActivePods(ctx); len(active) != 0 {
		t.Fatalf("expected 0 active pods after 4001 shutdown-machine close, got %d", len(active))
	}

	// 3. Test authorizeBearer variants on DELETE /api/v1/sessions/{ws_id}/{sub}:
	//    - derived signing_secret (base64url)
	//    - token_digest (sha256 hex)
	//    - valid signed ticket
	mid, _ := domain.NewMachineID("ws_multitab", "usr_multitab")
	signingSecretB64 := base64.RawURLEncoding.EncodeToString(ticket.DeriveSigningSecret(testClusterToken))
	sum := sha256.Sum256([]byte(testClusterToken))
	tokenDigestHex := hex.EncodeToString(sum[:])

	for _, bearer := range []string{signingSecretB64, tokenDigestHex, signedTicket} {
		_, _ = fakeK8s.EnsurePod(ctx, k8s.PodRequest{MachineID: mid, Tier: domain.TierStandard})
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, gwSrv.URL+"/api/v1/sessions/ws_multitab/usr_multitab", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatalf("DELETE session with bearer %q: %v", bearer[:8], doErr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE session with bearer %q status = %d, want 200", bearer[:8], resp.StatusCode)
		}
		if active, _ := fakeK8s.ListActivePods(ctx); len(active) != 0 {
			t.Fatalf("expected pod deleted with bearer %q", bearer[:8])
		}
	}

	// 4. Test direct ticket-authenticated shutdown on DELETE /ws/terminal?ticket=... and POST /ws/terminal?action=shutdown.
	for _, method := range []string{http.MethodDelete, http.MethodPost, http.MethodGet} {
		_, _ = fakeK8s.EnsurePod(ctx, k8s.PodRequest{MachineID: mid, Tier: domain.TierStandard})
		reqURL := gwSrv.URL + "/ws/terminal?ticket=" + signedTicket + "&action=shutdown"
		req, _ := http.NewRequestWithContext(ctx, method, reqURL, nil)
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			t.Fatalf("%s /ws/terminal shutdown: %v", method, doErr)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s /ws/terminal shutdown status = %d, want 200", method, resp.StatusCode)
		}
		if active, _ := fakeK8s.ListActivePods(ctx); len(active) != 0 {
			t.Fatalf("expected pod deleted after %s /ws/terminal shutdown", method)
		}
	}

	// Invalid ticket on DELETE /ws/terminal returns 401.
	badShutReq, _ := http.NewRequestWithContext(ctx, http.MethodDelete, gwSrv.URL+"/ws/terminal", nil)
	badShutReq.Header.Set("Authorization", "Bearer invalid-ticket")
	badShutResp, err := http.DefaultClient.Do(badShutReq)
	if err != nil {
		t.Fatalf("DELETE /ws/terminal invalid ticket: %v", err)
	}
	_ = badShutResp.Body.Close()
	if badShutResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid ticket on DELETE /ws/terminal, got %d", badShutResp.StatusCode)
	}
}
