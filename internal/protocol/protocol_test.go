package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/protocol"
)

func TestFrameCodec_RoundTrips(t *testing.T) {
	t.Parallel()

	codec, err := protocol.NewFrameCodec()
	if err != nil {
		t.Fatalf("NewFrameCodec error: %v", err)
	}

	t.Run("0x01 FrameInput", func(t *testing.T) {
		t.Parallel()
		input := []byte("\x1b[Ahello world\r\n")
		wire, err := codec.EncodeInput(input)
		if err != nil {
			t.Fatalf("EncodeInput error: %v", err)
		}
		if wire[0] != byte(protocol.FrameInput) {
			t.Fatalf("header = 0x%02x, want 0x01", wire[0])
		}
		dec, err := codec.Decode(wire)
		if err != nil {
			t.Fatalf("Decode error: %v", err)
		}
		if dec.Type != protocol.FrameInput || !bytes.Equal(dec.Data, input) {
			t.Errorf("decoded = %+v, want input %q", dec, input)
		}
	})

	t.Run("0x02 FrameResize big-endian uint16", func(t *testing.T) {
		t.Parallel()
		wire, err := codec.EncodeResize(132, 43)
		if err != nil {
			t.Fatalf("EncodeResize error: %v", err)
		}
		if len(wire) != 5 || wire[0] != byte(protocol.FrameResize) {
			t.Fatalf("unexpected resize wire frame: %x", wire)
		}
		// 132 = 0x0084, 43 = 0x002b
		if !bytes.Equal(wire[1:], []byte{0x00, 0x84, 0x00, 0x2b}) {
			t.Fatalf("big-endian bytes = %x, want 0084002b", wire[1:])
		}
		dec, err := codec.Decode(wire)
		if err != nil {
			t.Fatalf("Decode error: %v", err)
		}
		if dec.Type != protocol.FrameResize || dec.Cols != 132 || dec.Rows != 43 {
			t.Errorf("decoded resize = %+v, want 132x43", dec)
		}
	})

	t.Run("0x03 FramePing and 0x12 FramePong with and without timestamp", func(t *testing.T) {
		t.Parallel()
		pingWire, err := codec.EncodePing(1762264800123)
		if err != nil {
			t.Fatalf("EncodePing error: %v", err)
		}
		decPing, err := codec.Decode(pingWire)
		if err != nil {
			t.Fatalf("Decode ping error: %v", err)
		}
		if decPing.Type != protocol.FramePing || decPing.TimestampMS != 1762264800123 {
			t.Errorf("decoded ping = %+v", decPing)
		}

		// Empty ping payload (1 byte header only) is also valid per cross-repo contract
		emptyPing, err := codec.Decode([]byte{byte(protocol.FramePing)})
		if err != nil {
			t.Fatalf("Decode empty ping error: %v", err)
		}
		if emptyPing.Type != protocol.FramePing || emptyPing.TimestampMS != 0 {
			t.Errorf("decoded empty ping = %+v", emptyPing)
		}

		pongWire, err := codec.EncodePong(1762264800123)
		if err != nil {
			t.Fatalf("EncodePong error: %v", err)
		}
		decPong, err := codec.Decode(pongWire)
		if err != nil {
			t.Fatalf("Decode pong error: %v", err)
		}
		if decPong.Type != protocol.FramePong || decPong.TimestampMS != 1762264800123 {
			t.Errorf("decoded pong = %+v", decPong)
		}
	})

	t.Run("0x10 FrameSnapshot and 0x11 FrameDiff zstd compression", func(t *testing.T) {
		t.Parallel()
		ansiScreen := bytes.Repeat([]byte("\x1b[38;2;250;204;21mOrbit Machine Ready\x1b[0m\r\n"), 64)

		snapWire, err := codec.EncodeSnapshot(ansiScreen)
		if err != nil {
			t.Fatalf("EncodeSnapshot error: %v", err)
		}
		if snapWire[0] != byte(protocol.FrameSnapshot) {
			t.Fatalf("header = 0x%02x, want 0x10", snapWire[0])
		}
		if len(snapWire) >= len(ansiScreen) {
			t.Errorf("expected zstd compression to shrink repetitive ANSI screen (%d >= %d)", len(snapWire), len(ansiScreen))
		}
		decSnap, err := codec.Decode(snapWire)
		if err != nil {
			t.Fatalf("Decode snapshot error: %v", err)
		}
		if decSnap.Type != protocol.FrameSnapshot || !bytes.Equal(decSnap.Data, ansiScreen) {
			t.Errorf("snapshot round-trip mismatch")
		}

		diffBytes := []byte("\x1b[12;4H✦ Configure")
		diffWire, err := codec.EncodeDiff(diffBytes)
		if err != nil {
			t.Fatalf("EncodeDiff error: %v", err)
		}
		decDiff, err := codec.Decode(diffWire)
		if err != nil {
			t.Fatalf("Decode diff error: %v", err)
		}
		if decDiff.Type != protocol.FrameDiff || !bytes.Equal(decDiff.Data, diffBytes) {
			t.Errorf("diff round-trip mismatch")
		}
	})

	t.Run("0x13 FrameMetadata JSON", func(t *testing.T) {
		t.Parallel()
		meta := protocol.MetadataEvent{
			Type:        protocol.MetaEventBootProgress,
			Phase:       domain.PhaseHydratingProfile,
			Message:     "Restoring encrypted user profile from S3...",
			ProgressPct: 45,
			ActiveRepo:  "orbit",
			IdleSeconds: 0,
			ActiveTabs:  []string{"tab-1", "tab-2"},
			ClosedTabID: "tab-3",
		}
		wire, err := codec.EncodeMetadata(meta)
		if err != nil {
			t.Fatalf("EncodeMetadata error: %v", err)
		}
		dec, err := codec.Decode(wire)
		if err != nil {
			t.Fatalf("Decode metadata error: %v", err)
		}
		if dec.Type != protocol.FrameMetadata || dec.Metadata == nil || !reflect.DeepEqual(*dec.Metadata, meta) {
			t.Errorf("decoded metadata = %+v, want %+v", dec.Metadata, meta)
		}
	})

	t.Run("0x04 FrameFileUpload encode and decode", func(t *testing.T) {
		t.Parallel()
		content := []byte("package main\nfunc main() {}\n")
		wire, err := codec.EncodeUploadFrame("main.go", content)
		if err != nil {
			t.Fatalf("EncodeUploadFrame error: %v", err)
		}
		if wire[0] != byte(protocol.FrameFileUpload) {
			t.Fatalf("header = 0x%02x, want 0x04", wire[0])
		}
		dec, err := codec.Decode(wire)
		if err != nil {
			t.Fatalf("Decode upload frame error: %v", err)
		}
		if dec.Type != protocol.FrameFileUpload {
			t.Fatalf("decoded type = 0x%02x, want 0x04", dec.Type)
		}
		name, data, err := protocol.DecodeUploadPayload(dec.Data)
		if err != nil {
			t.Fatalf("DecodeUploadPayload(dec.Data) error: %v", err)
		}
		if name != "main.go" || !bytes.Equal(data, content) {
			t.Fatalf("got (%q, %q), want (\"main.go\", %q)", name, data, content)
		}

		// Also test DecodeUploadPayload directly with the 0x04 header prefix and unpadded base64
		name2, data2, err := protocol.DecodeUploadPayload(wire)
		if err != nil || name2 != "main.go" || !bytes.Equal(data2, content) {
			t.Fatalf("DecodeUploadPayload(wire) = (%q, %q, %v)", name2, data2, err)
		}
		unpadded := []byte(`{"name":"notes.txt","data":"aGVsbG8"}`)
		name3, data3, err := protocol.DecodeUploadPayload(unpadded)
		if err != nil || name3 != "notes.txt" || string(data3) != "hello" {
			t.Fatalf("DecodeUploadPayload(unpadded) = (%q, %q, %v)", name3, data3, err)
		}
	})
}

func TestFrameCodec_ValidationAndErrors(t *testing.T) {
	t.Parallel()

	if _, err := protocol.NewFrameCodec(protocol.WithMaxPayloadBytes(0)); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for maxPayloadBytes=0, got %v", err)
	}

	smallCodec, err := protocol.NewFrameCodec(protocol.WithMaxPayloadBytes(32))
	if err != nil {
		t.Fatalf("NewFrameCodec error: %v", err)
	}

	if _, err := smallCodec.EncodeResize(0, 24); !errors.Is(err, domain.ErrFrameInvalidResize) {
		t.Errorf("EncodeResize(0, 24) err = %v, want ErrFrameInvalidResize", err)
	}
	if _, err := smallCodec.EncodeResize(80, 0); !errors.Is(err, domain.ErrFrameInvalidResize) {
		t.Errorf("EncodeResize(80, 0) err = %v, want ErrFrameInvalidResize", err)
	}
	if _, err := smallCodec.EncodeInput(bytes.Repeat([]byte("a"), 64)); !errors.Is(err, domain.ErrFrameTooLarge) {
		t.Errorf("EncodeInput oversized err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := smallCodec.EncodeSnapshot(bytes.Repeat([]byte("a"), 64)); !errors.Is(err, domain.ErrFrameTooLarge) {
		t.Errorf("EncodeSnapshot oversized err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := smallCodec.EncodeDiff(bytes.Repeat([]byte("a"), 64)); !errors.Is(err, domain.ErrFrameTooLarge) {
		t.Errorf("EncodeDiff oversized err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := smallCodec.EncodeUploadFrame("big.bin", bytes.Repeat([]byte("a"), 64)); !errors.Is(err, domain.ErrFrameTooLarge) {
		t.Errorf("EncodeUploadFrame oversized err = %v, want ErrFrameTooLarge", err)
	}
	if _, err := smallCodec.EncodeUploadFrame("", []byte("x")); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Errorf("EncodeUploadFrame empty name err = %v, want ErrInvalidConfig", err)
	}
	if _, _, err := protocol.DecodeUploadPayload(nil); !errors.Is(err, domain.ErrFrameEmpty) {
		t.Errorf("DecodeUploadPayload(nil) err = %v, want ErrFrameEmpty", err)
	}
	if _, _, err := protocol.DecodeUploadPayload([]byte("{bad")); !errors.Is(err, protocol.ErrCorruptMetadataJSON) {
		t.Errorf("DecodeUploadPayload(bad json) err = %v, want ErrCorruptMetadataJSON", err)
	}
	if _, _, err := protocol.DecodeUploadPayload([]byte(`{"name":"","data":"YQ=="}`)); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Errorf("DecodeUploadPayload(empty name) err = %v, want ErrInvalidConfig", err)
	}
	if _, _, err := protocol.DecodeUploadPayload([]byte(`{"name":"a.txt","data":"!!!not-base64!!!"}`)); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Errorf("DecodeUploadPayload(bad base64) err = %v, want ErrInvalidConfig", err)
	}

	// Build a zstd bomb that compresses to <32 bytes on wire but expands >32 bytes on decode
	largeCodec, _ := protocol.NewFrameCodec(protocol.WithMaxPayloadBytes(4096))
	compressedRepeat, err := largeCodec.EncodeSnapshot(bytes.Repeat([]byte("Z"), 200))
	if err != nil {
		t.Fatalf("largeCodec.EncodeSnapshot error: %v", err)
	}

	decodeTests := []struct {
		name    string
		raw     []byte
		wantErr error
	}{
		{
			name:    "empty frame",
			raw:     nil,
			wantErr: domain.ErrFrameEmpty,
		},
		{
			name:    "oversized wire frame",
			raw:     bytes.Repeat([]byte{byte(protocol.FrameInput)}, 64),
			wantErr: domain.ErrFrameTooLarge,
		},
		{
			name:    "zstd payload expands beyond maxPayloadBytes",
			raw:     compressedRepeat,
			wantErr: domain.ErrFrameTooLarge,
		},
		{
			name:    "unknown frame type",
			raw:     []byte{0x99, 0x01},
			wantErr: domain.ErrFrameUnknownType,
		},
		{
			name:    "resize wrong byte length",
			raw:     []byte{byte(protocol.FrameResize), 0x00, 0x50},
			wantErr: domain.ErrFrameInvalidResize,
		},
		{
			name:    "resize zero dimension on wire",
			raw:     []byte{byte(protocol.FrameResize), 0x00, 0x00, 0x00, 0x18},
			wantErr: domain.ErrFrameInvalidResize,
		},
		{
			name:    "ping invalid payload length (neither 0 nor 8)",
			raw:     []byte{byte(protocol.FramePing), 0x01, 0x02},
			wantErr: domain.ErrFrameUnknownType,
		},
		{
			name:    "corrupted zstd snapshot",
			raw:     []byte{byte(protocol.FrameSnapshot), 0xff, 0xfe, 0xfd},
			wantErr: protocol.ErrCorruptCompressedFrame,
		},
		{
			name:    "corrupted metadata json",
			raw:     []byte{byte(protocol.FrameMetadata), '{', 'b', 'a', 'd'},
			wantErr: protocol.ErrCorruptMetadataJSON,
		},
	}

	for _, tc := range decodeTests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := smallCodec.Decode(tc.raw)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Decode(%x) err = %v, want %v", tc.raw, err, tc.wantErr)
			}
		})
	}
}

func TestControlPlaneDTOs_JSONSerialization(t *testing.T) {
	t.Parallel()

	hb := protocol.HeartbeatRequest{
		ActivePods:        1,
		MaxConcurrentPods: 25,
		Sessions: []protocol.SessionStatus{
			{
				WorkspaceID: "ws_99",
				Subject:     "usr_42",
				PodName:     "machine-ws-99-usr-42",
				Phase:       domain.PhaseReady,
				Tier:        domain.TierPerformance,
				IdleSeconds: 14,
			},
		},
	}
	raw, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("json.Marshal(HeartbeatRequest) error: %v", err)
	}
	var roundTrip protocol.HeartbeatRequest
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatalf("json.Unmarshal(HeartbeatRequest) error: %v", err)
	}
	if len(roundTrip.Sessions) != 1 || roundTrip.Sessions[0].PodName != "machine-ws-99-usr-42" {
		t.Errorf("unexpected roundTrip: %+v", roundTrip)
	}
}
