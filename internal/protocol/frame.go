package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/davidschrooten/orbit-machines/internal/domain"
)

// UploadPayload is the JSON payload carried inside 0x04 FrameFileUpload messages
// and POST /api/v1/upload requests.
type UploadPayload struct {
	Name string `json:"name"`
	Data string `json:"data"` // base64-encoded file bytes
}

// EncodeUploadFrame encodes a filename and raw file bytes into a 0x04 FrameFileUpload binary frame.
func EncodeUploadFrame(name string, data []byte) ([]byte, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" || trimmed == "." || trimmed == ".." {
		return nil, fmt.Errorf("%w: upload filename is required", domain.ErrInvalidConfig)
	}
	rawJSON, err := json.Marshal(UploadPayload{
		Name: trimmed,
		Data: base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling upload payload: %w", err)
	}
	out := make([]byte, 1+len(rawJSON))
	out[0] = byte(FrameFileUpload)
	copy(out[1:], rawJSON)
	return out, nil
}

// EncodeUploadFrame builds a 0x04 FrameFileUpload binary message using the codec's payload size limit.
func (c *FrameCodec) EncodeUploadFrame(name string, data []byte) ([]byte, error) {
	wire, err := EncodeUploadFrame(name, data)
	if err != nil {
		return nil, err
	}
	if len(wire)-1 > c.maxPayloadBytes {
		return nil, fmt.Errorf("%w: upload payload %d > %d", domain.ErrFrameTooLarge, len(wire)-1, c.maxPayloadBytes)
	}
	return wire, nil
}

// DecodeUploadPayload decodes a 0x04 FrameFileUpload payload (with or without the leading 0x04 header byte)
// and returns the filename and raw file bytes.
func DecodeUploadPayload(payload []byte) (string, []byte, error) {
	if len(payload) == 0 {
		return "", nil, domain.ErrFrameEmpty
	}
	if payload[0] == byte(FrameFileUpload) {
		payload = payload[1:]
	}
	var up UploadPayload
	if err := json.Unmarshal(payload, &up); err != nil {
		return "", nil, fmt.Errorf("%w: %w", ErrCorruptMetadataJSON, err)
	}
	name := strings.TrimSpace(up.Name)
	if name == "" || name == "." || name == ".." {
		return "", nil, fmt.Errorf("%w: upload filename is required", domain.ErrInvalidConfig)
	}
	decoded, err := base64.StdEncoding.DecodeString(up.Data)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(up.Data)
		if err != nil {
			return "", nil, fmt.Errorf("%w: invalid base64 upload data: %w", domain.ErrInvalidConfig, err)
		}
	}
	return name, decoded, nil
}
