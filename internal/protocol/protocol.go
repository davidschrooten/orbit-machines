// Package protocol implements the binary WebSocket terminal framing codec
// (0x01-0x13) and REST control-plane DTOs shared with the Orbit monolith.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/klauspost/compress/zstd"
)

// DefaultMaxPayloadBytes is the default upper bound (4 MiB) on uncompressed or
// wire WebSocket payloads.
const DefaultMaxPayloadBytes = 4 << 20

// Protocol-specific errors.
var (
	ErrCorruptCompressedFrame = errors.New("corrupt zstd compressed frame payload")
	ErrCorruptMetadataJSON    = errors.New("corrupt metadata frame json payload")
)

// FrameType is the 1-byte header prefix on every binary WebSocket message.
type FrameType byte

const (
	// Client -> Server frames
	FrameInput  FrameType = 0x01
	FrameResize FrameType = 0x02
	FramePing   FrameType = 0x03

	// Server -> Client frames
	FrameSnapshot FrameType = 0x10
	FrameDiff     FrameType = 0x11
	FramePong     FrameType = 0x12
	FrameMetadata FrameType = 0x13
)

// MetadataEventType classifies out-of-band 0x13 metadata events.
type MetadataEventType string

const (
	MetaEventBootProgress MetadataEventType = "boot_progress"
	MetaEventIdleWarning  MetadataEventType = "idle_warning"
	MetaEventSessionState MetadataEventType = "session_state"
	MetaEventWIPPrompt    MetadataEventType = "wip_restore_prompt"
)

// MetadataEvent is serialized as UTF-8 JSON inside 0x13 FrameMetadata messages.
type MetadataEvent struct {
	Type         MetadataEventType `json:"type"`
	Phase        domain.BootPhase  `json:"phase,omitempty"`
	Message      string            `json:"message,omitempty"`
	ProgressPct  int               `json:"progress_pct,omitempty"`
	ActiveRepo   string            `json:"active_repo,omitempty"`
	ActiveIssue  string            `json:"active_issue,omitempty"`
	IdleSeconds  int               `json:"idle_seconds,omitempty"`
	HasWIPBundle bool              `json:"has_wip_bundle,omitempty"`
}

// DecodedFrame represents a parsed binary WebSocket message.
type DecodedFrame struct {
	Type        FrameType
	Data        []byte
	Cols        uint16
	Rows        uint16
	TimestampMS uint64
	Metadata    *MetadataEvent
}

// CodecOption configures a FrameCodec.
type CodecOption func(*FrameCodec) error

// WithMaxPayloadBytes sets the maximum allowed payload size in bytes.
func WithMaxPayloadBytes(maxBytes int) CodecOption {
	return func(c *FrameCodec) error {
		if maxBytes <= 0 {
			return fmt.Errorf("%w: maxPayloadBytes must be positive, got %d", domain.ErrInvalidConfig, maxBytes)
		}
		c.maxPayloadBytes = maxBytes
		return nil
	}
}

// FrameCodec encodes and decodes binary WebSocket terminal frames with reusable,
// concurrency-safe zstd encoders and decoders.
type FrameCodec struct {
	maxPayloadBytes int
	encoder         *zstd.Encoder
	decoder         *zstd.Decoder
}

// NewFrameCodec creates a concurrency-safe FrameCodec.
func NewFrameCodec(opts ...CodecOption) (*FrameCodec, error) {
	c := &FrameCodec{
		maxPayloadBytes: DefaultMaxPayloadBytes,
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}

	maxDecoderMem := uint64(c.maxPayloadBytes) * 2
	if maxDecoderMem < 8<<20 {
		maxDecoderMem = 8 << 20
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	dec, _ := zstd.NewReader(
		nil,
		zstd.WithDecoderMaxMemory(maxDecoderMem),
	)
	c.encoder = enc
	c.decoder = dec
	return c, nil
}

// EncodeInput builds a 0x01 FrameInput binary message.
func (c *FrameCodec) EncodeInput(data []byte) ([]byte, error) {
	if len(data) > c.maxPayloadBytes {
		return nil, fmt.Errorf("%w: input length %d > %d", domain.ErrFrameTooLarge, len(data), c.maxPayloadBytes)
	}
	out := make([]byte, 1+len(data))
	out[0] = byte(FrameInput)
	copy(out[1:], data)
	return out, nil
}

// EncodeResize builds a 5-byte 0x02 FrameResize binary message (<0x02><uint16-be-cols><uint16-be-rows>).
func (c *FrameCodec) EncodeResize(cols, rows uint16) ([]byte, error) {
	if cols == 0 || rows == 0 {
		return nil, fmt.Errorf("%w: cols (%d) and rows (%d) must be non-zero", domain.ErrFrameInvalidResize, cols, rows)
	}
	out := make([]byte, 5)
	out[0] = byte(FrameResize)
	binary.BigEndian.PutUint16(out[1:3], cols)
	binary.BigEndian.PutUint16(out[3:5], rows)
	return out, nil
}

// EncodePing builds a 9-byte 0x03 FramePing binary message with a 64-bit big-endian client timestamp.
func (c *FrameCodec) EncodePing(clientTimestampMS uint64) ([]byte, error) {
	return encodeTimestampFrame(FramePing, clientTimestampMS), nil
}

// EncodePong builds a 9-byte 0x12 FramePong binary message echoing the client timestamp.
func (c *FrameCodec) EncodePong(clientTimestampMS uint64) ([]byte, error) {
	return encodeTimestampFrame(FramePong, clientTimestampMS), nil
}

// EncodeSnapshot zstd-compresses a full terminal screen and prepends 0x10.
func (c *FrameCodec) EncodeSnapshot(ansi []byte) ([]byte, error) {
	return c.encodeCompressed(FrameSnapshot, ansi)
}

// EncodeDiff zstd-compresses a dirty-screen diff and prepends 0x11.
func (c *FrameCodec) EncodeDiff(ansi []byte) ([]byte, error) {
	return c.encodeCompressed(FrameDiff, ansi)
}

// EncodeMetadata JSON-marshals a MetadataEvent and prepends 0x13.
func (c *FrameCodec) EncodeMetadata(event MetadataEvent) ([]byte, error) {
	rawJSON, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata event: %w", err)
	}
	out := make([]byte, 1+len(rawJSON))
	out[0] = byte(FrameMetadata)
	copy(out[1:], rawJSON)
	return out, nil
}

// Decode parses a raw binary WebSocket message according to its 1-byte header.
func (c *FrameCodec) Decode(raw []byte) (DecodedFrame, error) {
	if len(raw) == 0 {
		return DecodedFrame{}, domain.ErrFrameEmpty
	}
	payload := raw[1:]
	if len(payload) > c.maxPayloadBytes {
		return DecodedFrame{}, fmt.Errorf("%w: wire payload %d > %d", domain.ErrFrameTooLarge, len(payload), c.maxPayloadBytes)
	}

	ft := FrameType(raw[0])
	switch ft {
	case FrameInput:
		buf := make([]byte, len(payload))
		copy(buf, payload)
		return DecodedFrame{Type: FrameInput, Data: buf}, nil

	case FrameResize:
		if len(payload) != 4 {
			return DecodedFrame{}, fmt.Errorf("%w: expected 4 bytes, got %d", domain.ErrFrameInvalidResize, len(payload))
		}
		cols := binary.BigEndian.Uint16(payload[0:2])
		rows := binary.BigEndian.Uint16(payload[2:4])
		if cols == 0 || rows == 0 {
			return DecodedFrame{}, fmt.Errorf("%w: cols (%d) and rows (%d) must be non-zero", domain.ErrFrameInvalidResize, cols, rows)
		}
		return DecodedFrame{Type: FrameResize, Cols: cols, Rows: rows}, nil

	case FramePing, FramePong:
		var ts uint64
		switch len(payload) {
		case 0:
			ts = 0
		case 8:
			ts = binary.BigEndian.Uint64(payload)
		default:
			return DecodedFrame{}, fmt.Errorf("%w: ping/pong payload must be 0 or 8 bytes, got %d", domain.ErrFrameUnknownType, len(payload))
		}
		return DecodedFrame{Type: ft, TimestampMS: ts}, nil

	case FrameSnapshot, FrameDiff:
		decompressed, err := c.decoder.DecodeAll(payload, nil)
		if err != nil {
			if errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded) {
				return DecodedFrame{}, fmt.Errorf("%w: %w", domain.ErrFrameTooLarge, err)
			}
			return DecodedFrame{}, fmt.Errorf("%w: %w", ErrCorruptCompressedFrame, err)
		}
		if len(decompressed) > c.maxPayloadBytes {
			return DecodedFrame{}, fmt.Errorf("%w: decompressed length %d > %d", domain.ErrFrameTooLarge, len(decompressed), c.maxPayloadBytes)
		}
		return DecodedFrame{Type: ft, Data: decompressed}, nil

	case FrameMetadata:
		var meta MetadataEvent
		if err := json.Unmarshal(payload, &meta); err != nil {
			return DecodedFrame{}, fmt.Errorf("%w: %w", ErrCorruptMetadataJSON, err)
		}
		return DecodedFrame{Type: FrameMetadata, Metadata: &meta}, nil

	default:
		return DecodedFrame{}, fmt.Errorf("%w: 0x%02x", domain.ErrFrameUnknownType, byte(ft))
	}
}

func (c *FrameCodec) encodeCompressed(ft FrameType, ansi []byte) ([]byte, error) {
	if len(ansi) > c.maxPayloadBytes {
		return nil, fmt.Errorf("%w: uncompressed length %d > %d", domain.ErrFrameTooLarge, len(ansi), c.maxPayloadBytes)
	}
	dst := make([]byte, 1, 1+len(ansi)/2+16)
	dst[0] = byte(ft)
	compressed := c.encoder.EncodeAll(ansi, dst[1:])
	return append(dst[:1], compressed...), nil
}

func encodeTimestampFrame(ft FrameType, ts uint64) []byte {
	out := make([]byte, 9)
	out[0] = byte(ft)
	binary.BigEndian.PutUint64(out[1:9], ts)
	return out
}

// RegisterClusterRequest is sent by orbit-dispatcher to POST /api/v1/machine_clusters/register.
type RegisterClusterRequest struct {
	ClusterVersion      string `json:"cluster_version"`
	DispatcherURL       string `json:"dispatcher_url"`
	IngressHost         string `json:"ingress_host"`
	KubernetesNamespace string `json:"kubernetes_namespace"`
	MinIOEnabled        bool   `json:"minio_enabled"`
	MaxConcurrentPods   int    `json:"max_concurrent_pods"`
}

// RegisterClusterResponse is returned by Orbit when a cluster registers.
type RegisterClusterResponse struct {
	ClusterID                string `json:"cluster_id"`
	WorkspaceID              string `json:"workspace_id"`
	WorkspaceSlug            string `json:"workspace_slug"`
	Status                   string `json:"status"`
	HeartbeatIntervalSeconds int    `json:"heartbeat_interval_seconds"`
}

// SessionStatus reports a running Machine Pod in periodic cluster heartbeats.
type SessionStatus struct {
	WorkspaceID string              `json:"ws_id"`
	Subject     string              `json:"sub"`
	PodName     string              `json:"pod_name"`
	Phase       domain.BootPhase    `json:"phase"`
	Tier        domain.ResourceTier `json:"tier"`
	IdleSeconds int                 `json:"idle_seconds"`
}

// HeartbeatRequest is sent by orbit-dispatcher to POST /api/v1/machine_clusters/heartbeat.
type HeartbeatRequest struct {
	ActivePods        int             `json:"active_pods"`
	MaxConcurrentPods int             `json:"max_concurrent_pods"`
	Sessions          []SessionStatus `json:"sessions"`
}

// HeartbeatResponse is returned by Orbit on each heartbeat.
type HeartbeatResponse struct {
	Status         string   `json:"status"`
	RevokeSessions []string `json:"revoke_sessions"`
}

// MintCredentialsRequest asks Orbit for short-lived MCP and Git credentials for a booting Pod.
type MintCredentialsRequest struct {
	WorkspaceID string   `json:"ws_id"`
	Subject     string   `json:"sub"`
	RepoIDs     []string `json:"repo_ids"`
}

// RepoCredential contains a repository clone URL and short-lived token.
type RepoCredential struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch"`
	GitUsername   string    `json:"git_username"`
	GitToken      string    `json:"git_token"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// MintCredentialsResponse is returned by POST /api/v1/machine_clusters/credentials.
type MintCredentialsResponse struct {
	OrbitMCPToken string           `json:"orbit_mcp_token"`
	Repositories  []RepoCredential `json:"repositories"`
}
