// Package domain defines pure domain value types, invariants, and sentinel errors
// for Orbit Machines without external infrastructure dependencies.
package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Sentinel domain errors across Orbit Machines.
var (
	ErrInvalidMachineID    = errors.New("invalid machine identifier")
	ErrInvalidResourceTier = errors.New("invalid resource tier")
	ErrTicketMalformed     = errors.New("malformed session ticket")
	ErrTicketSignature     = errors.New("invalid session ticket signature")
	ErrTicketExpired       = errors.New("session ticket expired")
	ErrTicketInvalidClaims = errors.New("invalid session ticket claims")
	ErrFrameEmpty          = errors.New("empty websocket frame")
	ErrFrameUnknownType    = errors.New("unknown websocket frame type")
	ErrFrameInvalidResize  = errors.New("invalid resize frame payload")
	ErrFrameTooLarge       = errors.New("websocket frame exceeds maximum size")
	ErrInvalidConfig       = errors.New("invalid configuration")
)

// MachineID uniquely identifies a user's ephemeral machine within an Orbit workspace.
type MachineID struct {
	workspaceID string
	userID      string
}

// NewMachineID validates and constructs a MachineID for a (workspaceID, userID) pair.
func NewMachineID(workspaceID, userID string) (MachineID, error) {
	ws := strings.TrimSpace(workspaceID)
	usr := strings.TrimSpace(userID)
	if ws == "" || usr == "" {
		return MachineID{}, fmt.Errorf("%w: workspaceID and userID are required", ErrInvalidMachineID)
	}
	if !isSafeIdentifierSegment(ws) || !isSafeIdentifierSegment(usr) {
		return MachineID{}, fmt.Errorf("%w: identifiers must not contain path separators or traversal segments", ErrInvalidMachineID)
	}
	return MachineID{
		workspaceID: ws,
		userID:      usr,
	}, nil
}

// WorkspaceID returns the Orbit workspace identifier.
func (m MachineID) WorkspaceID() string { return m.workspaceID }

// UserID returns the Orbit user identifier.
func (m MachineID) UserID() string { return m.userID }

// PodName returns a deterministic RFC 1123 Kubernetes Pod name (<= 63 chars).
// When the raw identifiers strictly consist of lowercase ASCII letters, digits,
// hyphens, or underscores and fit within 63 characters, it produces
// "machine-<ws>-<user>". Otherwise it appends an 8-char SHA-256 digest suffix
// to prevent collisions across truncated or case-folded identifiers.
func (m MachineID) PodName() string {
	wsClean, wsLossless := sanitizeDNSFragment(m.workspaceID)
	usrClean, usrLossless := sanitizeDNSFragment(m.userID)
	candidate := fmt.Sprintf("machine-%s-%s", wsClean, usrClean)

	if wsLossless && usrLossless && len(candidate) <= 63 {
		return candidate
	}

	sum := sha256.Sum256([]byte(m.workspaceID + "\x00" + m.userID))
	suffix := hex.EncodeToString(sum[:4]) // 8 hex chars

	const maxPrefixLen = 63 - 1 - 8 // 54 chars for prefix before "-<8hex>"
	prefix := candidate
	if len(prefix) > maxPrefixLen {
		prefix = strings.TrimRight(prefix[:maxPrefixLen], "-")
	}
	return prefix + "-" + suffix
}

// ProfileObjectKey returns the S3/MinIO object key for the user's encrypted profile archive.
func (m MachineID) ProfileObjectKey() string {
	return fmt.Sprintf("profiles/%s/%s/profile.tar.zst.enc", m.workspaceID, m.userID)
}

// WIPBundleObjectKey returns the S3/MinIO object key for an idle-evicted WIP git bundle.
func (m MachineID) WIPBundleObjectKey(repoID string) string {
	return fmt.Sprintf("profiles/%s/%s/wip/%s.bundle.tar.zst.enc", m.workspaceID, m.userID, repoID)
}

func isSafeIdentifierSegment(s string) bool {
	if s == "." || s == ".." || strings.Contains(s, "..") {
		return false
	}
	return !strings.ContainsAny(s, "/\\\x00")
}

func sanitizeDNSFragment(raw string) (cleaned string, lossless bool) {
	lossless = true
	var b strings.Builder
	b.Grow(len(raw))
	lastHyphen := false

	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9'):
			b.WriteByte(ch)
			lastHyphen = false
		case ch == '-' || ch == '_':
			if lastHyphen || b.Len() == 0 {
				lossless = false
				continue
			}
			b.WriteByte('-')
			lastHyphen = true
		case ch >= 'A' && ch <= 'Z':
			lossless = false
			b.WriteByte(ch + ('a' - 'A'))
			lastHyphen = false
		default:
			lossless = false
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}

	res := strings.Trim(b.String(), "-")
	if res == "" {
		return "id", false
	}
	return res, lossless
}

// ResourceTier defines the compute sizing tier granted to a user's Machine Pod.
type ResourceTier string

// Supported Machine Pod resource sizing tiers.
const (
	TierStandard    ResourceTier = "standard"
	TierPerformance ResourceTier = "performance"
)

// ResourceSpecs holds Kubernetes CPU, memory, and ephemeral-storage resource quantities.
type ResourceSpecs struct {
	CPURequest            string
	CPULimit              string
	MemoryRequest         string
	MemoryLimit           string
	EphemeralStorageLimit string
}

// ParseResourceTier parses and validates a ResourceTier string.
func ParseResourceTier(raw string) (ResourceTier, error) {
	switch ResourceTier(strings.TrimSpace(raw)) {
	case TierStandard:
		return TierStandard, nil
	case TierPerformance:
		return TierPerformance, nil
	default:
		return "", fmt.Errorf("%w: %q (must be %q or %q)", ErrInvalidResourceTier, raw, TierStandard, TierPerformance)
	}
}

// Specs returns the Kubernetes resource requests and limits for the tier.
func (t ResourceTier) Specs() ResourceSpecs {
	switch t {
	case TierPerformance:
		return ResourceSpecs{
			CPURequest:            "2000m",
			CPULimit:              "4000m",
			MemoryRequest:         "4Gi",
			MemoryLimit:           "8Gi",
			EphemeralStorageLimit: "40Gi",
		}
	default:
		return ResourceSpecs{
			CPURequest:            "1000m",
			CPULimit:              "2000m",
			MemoryRequest:         "2Gi",
			MemoryLimit:           "4Gi",
			EphemeralStorageLimit: "20Gi",
		}
	}
}

// BootPhase represents the lifecycle stage of a Machine Pod during provisioning.
type BootPhase string

// Supported Machine Pod boot lifecycle phases.
const (
	PhaseScheduling       BootPhase = "scheduling"
	PhaseHydratingProfile BootPhase = "hydrating_profile"
	PhaseEvaluatingFlake  BootPhase = "evaluating_flake"
	PhaseReady            BootPhase = "ready"
	PhaseStopping         BootPhase = "stopping"
	PhaseStopped          BootPhase = "stopped"
)

// WorkspaceRepo describes a git repository registered in an Orbit workspace.
type WorkspaceRepo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
}
