// Package ticket implements signing and verification of short-lived browser
// session tickets shared between Orbit Rails and orbit-dispatcher.
package ticket

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
)

const (
	// VersionPrefix is the wire format version header for session tickets.
	VersionPrefix = "v1"
	// ExpectedIssuer is the mandatory issuer claim set by the Orbit monolith.
	ExpectedIssuer = "orbit"
	// MaxLifetimeSeconds is the maximum allowed duration (exp - iat) of a ticket.
	MaxLifetimeSeconds = 120
	// MaxClockSkewSeconds is the maximum tolerated future iat skew during verification.
	MaxClockSkewSeconds = 15
	// MinSecretLength is the minimum byte length for a raw signing secret.
	MinSecretLength = 16
	// KeyDerivationContext is the domain-separation label used to derive a
	// 32-byte ticket signing key from a cluster bearer token.
	KeyDerivationContext = "orbit-machines:session-ticket:v1"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Claims represents the authenticated payload inside a v1 session ticket.
type Claims struct {
	JTI           string              `json:"jti"`
	Issuer        string              `json:"iss"`
	Subject       string              `json:"sub"`
	WorkspaceID   string              `json:"ws_id"`
	WorkspaceSlug string              `json:"ws_slug"`
	UserLogin     string              `json:"user_login"`
	Tier          domain.ResourceTier `json:"tier"`
	OrbitURL      string              `json:"orbit_url,omitempty"`
	MCPToken      string              `json:"mcp_token,omitempty"`
	IssuedAt      int64               `json:"iat"`
	ExpiresAt     int64               `json:"exp"`
}

// MachineID constructs the validated domain.MachineID from the ticket claims.
func (c Claims) MachineID() (domain.MachineID, error) {
	return domain.NewMachineID(c.WorkspaceID, c.Subject)
}

// ValidateStatic checks time-independent claim invariants.
func (c Claims) ValidateStatic() error {
	if strings.TrimSpace(c.JTI) == "" {
		return fmt.Errorf("%w: jti is required", domain.ErrTicketInvalidClaims)
	}
	if c.Issuer != ExpectedIssuer {
		return fmt.Errorf("%w: unexpected issuer %q", domain.ErrTicketInvalidClaims, c.Issuer)
	}
	if _, err := c.MachineID(); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrTicketInvalidClaims, err)
	}
	if !slugPattern.MatchString(c.WorkspaceSlug) {
		return fmt.Errorf("%w: invalid ws_slug %q", domain.ErrTicketInvalidClaims, c.WorkspaceSlug)
	}
	if !slugPattern.MatchString(c.UserLogin) {
		return fmt.Errorf("%w: invalid user_login %q", domain.ErrTicketInvalidClaims, c.UserLogin)
	}
	if _, err := domain.ParseResourceTier(string(c.Tier)); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrTicketInvalidClaims, err)
	}
	if c.ExpiresAt <= c.IssuedAt {
		return fmt.Errorf("%w: exp (%d) must be greater than iat (%d)", domain.ErrTicketInvalidClaims, c.ExpiresAt, c.IssuedAt)
	}
	if c.ExpiresAt-c.IssuedAt > MaxLifetimeSeconds {
		return fmt.Errorf("%w: ticket lifetime (%ds) exceeds maximum (%ds)", domain.ErrTicketInvalidClaims, c.ExpiresAt-c.IssuedAt, MaxLifetimeSeconds)
	}
	return nil
}

// Validate checks both static claim invariants and temporal validity at now.
func (c Claims) Validate(now time.Time) error {
	if err := c.ValidateStatic(); err != nil {
		return err
	}
	unixNow := now.Unix()
	if unixNow > c.ExpiresAt {
		return fmt.Errorf("%w: expired at %d (now %d)", domain.ErrTicketExpired, c.ExpiresAt, unixNow)
	}
	if c.IssuedAt > unixNow+MaxClockSkewSeconds {
		return fmt.Errorf("%w: iat %d is in the future (now %d)", domain.ErrTicketInvalidClaims, c.IssuedAt, unixNow)
	}
	return nil
}

// Codec signs and verifies v1 session tickets using HMAC-SHA256.
type Codec struct {
	secret []byte
}

// NewCodec constructs a Codec from a raw secret of at least MinSecretLength bytes.
func NewCodec(secret []byte) (*Codec, error) {
	if len(secret) < MinSecretLength {
		return nil, fmt.Errorf("%w: ticket signing secret must be at least %d bytes", domain.ErrInvalidConfig, MinSecretLength)
	}
	keyCopy := make([]byte, len(secret))
	copy(keyCopy, secret)
	return &Codec{secret: keyCopy}, nil
}

// DeriveSigningSecret derives the deterministic 32-byte HMAC signing key from
// a shared cluster bearer token ("omc_live_...") using HMAC-SHA256 key derivation.
func DeriveSigningSecret(clusterToken string) []byte {
	trimmed := strings.TrimSpace(clusterToken)
	mac := hmac.New(sha256.New, []byte(KeyDerivationContext))
	_, _ = mac.Write([]byte(trimmed))
	return mac.Sum(nil)
}

// NewCodecFromClusterToken derives a deterministic 32-byte HMAC signing key from
// the shared cluster bearer token ("omc_live_...") using HMAC-SHA256 key derivation.
func NewCodecFromClusterToken(clusterToken string) (*Codec, error) {
	trimmed := strings.TrimSpace(clusterToken)
	if len(trimmed) < MinSecretLength {
		return nil, fmt.Errorf("%w: cluster token must be at least %d characters", domain.ErrInvalidConfig, MinSecretLength)
	}
	return &Codec{secret: DeriveSigningSecret(trimmed)}, nil
}

// Sign validates static claim invariants and produces a signed "v1.<payload>.<sig>" ticket.
func (c *Codec) Sign(claims Claims) (string, error) {
	if err := claims.ValidateStatic(); err != nil {
		return "", err
	}
	rawJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshaling ticket claims: %w", err)
	}
	return c.signPayloadBytes(rawJSON), nil
}

// Verify checks the wire structure, HMAC-SHA256 signature, and claim validity at now.
func (c *Codec) Verify(token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != VersionPrefix {
		return Claims{}, fmt.Errorf("%w: expected %s.<payload>.<sig>", domain.ErrTicketMalformed, VersionPrefix)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: decoding signature: %w", domain.ErrTicketMalformed, err)
	}

	signingInput := parts[0] + "." + parts[1]
	expectedSig := c.computeMAC(signingInput)
	if !hmac.Equal(sigBytes, expectedSig) {
		return Claims{}, domain.ErrTicketSignature
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: decoding payload: %w", domain.ErrTicketMalformed, err)
	}

	var claims Claims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return Claims{}, fmt.Errorf("%w: unmarshaling payload json: %w", domain.ErrTicketMalformed, err)
	}

	if err := claims.Validate(now); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func (c *Codec) signPayloadBytes(payload []byte) string {
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	signingInput := VersionPrefix + "." + encodedPayload
	sig := base64.RawURLEncoding.EncodeToString(c.computeMAC(signingInput))
	return signingInput + "." + sig
}

func (c *Codec) computeMAC(signingInput string) []byte {
	mac := hmac.New(sha256.New, c.secret)
	_, _ = mac.Write([]byte(signingInput))
	return mac.Sum(nil)
}
