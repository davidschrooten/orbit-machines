package ticket_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/ticket"
)

func TestCodec_SignAndVerify_GoldenVector(t *testing.T) {
	t.Parallel()

	clusterToken := "omc_live_0123456789abcdef0123456789abcdef"
	codec, err := ticket.NewCodecFromClusterToken(clusterToken)
	if err != nil {
		t.Fatalf("NewCodecFromClusterToken unexpected error: %v", err)
	}

	claims := ticket.Claims{
		JTI:            "01960000-0000-7000-8000-000000000001",
		Issuer:         ticket.ExpectedIssuer,
		Subject:        "usr_42",
		WorkspaceID:    "ws_99",
		WorkspaceSlug:  "acme",
		UserLogin:      "david",
		Tier:           domain.TierPerformance,
		IssuedAt:       1762264800,
		ExpiresAt:      1762264860,
	}

	token, err := codec.Sign(claims)
	if err != nil {
		t.Fatalf("Sign unexpected error: %v", err)
	}

	// Verify deterministic golden wire prefix and 3-part structure
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("unexpected token structure: %q", token)
	}

	now := time.Unix(1762264830, 0)
	got, err := codec.Verify(token, now)
	if err != nil {
		t.Fatalf("Verify unexpected error: %v", err)
	}
	if got != claims {
		t.Errorf("Verify claims = %+v, want %+v", got, claims)
	}

	mid, err := got.MachineID()
	if err != nil {
		t.Fatalf("MachineID() unexpected error: %v", err)
	}
	if mid.PodName() != "machine-ws-99-usr-42" {
		t.Errorf("PodName() = %q, want machine-ws-99-usr-42", mid.PodName())
	}
}

func TestNewCodec_Validation(t *testing.T) {
	t.Parallel()

	if _, err := ticket.NewCodec([]byte("short")); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for short key, got %v", err)
	}
	if _, err := ticket.NewCodecFromClusterToken("   "); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty cluster token, got %v", err)
	}
}

func TestCodec_Verify_RejectsTamperingAndMalformedTokens(t *testing.T) {
	t.Parallel()

	secret := []byte("0123456789abcdef0123456789abcdef")
	codec, err := ticket.NewCodec(secret)
	if err != nil {
		t.Fatalf("NewCodec error: %v", err)
	}

	validClaims := ticket.Claims{
		JTI:           "jti-1",
		Issuer:        "orbit",
		Subject:       "usr_1",
		WorkspaceID:   "ws_1",
		WorkspaceSlug: "orbit-hq",
		UserLogin:     "alice",
		Tier:          domain.TierStandard,
		IssuedAt:      1000,
		ExpiresAt:     1060,
	}
	validToken, err := codec.Sign(validClaims)
	if err != nil {
		t.Fatalf("Sign error: %v", err)
	}
	parts := strings.Split(validToken, ".")

	otherCodec, _ := ticket.NewCodec([]byte("fedcba9876543210fedcba9876543210"))
	wrongKeyToken, _ := otherCodec.Sign(validClaims)

	tests := []struct {
		name    string
		token   string
		now     time.Time
		wantErr error
	}{
		{
			name:    "missing segments",
			token:   "v1.onlytwo",
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketMalformed,
		},
		{
			name:    "unsupported version prefix",
			token:   "v2." + parts[1] + "." + parts[2],
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketMalformed,
		},
		{
			name:    "invalid base64 signature",
			token:   "v1." + parts[1] + ".***not-base64***",
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketMalformed,
		},
		{
			name:    "invalid json payload with valid signature",
			token:   codec.SignRawForTest([]byte("not-json")),
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketMalformed,
		},
		{
			name:    "invalid base64 payload with valid signature",
			token:   codec.SignCustomSegmentForTest("***not-base64***"),
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketMalformed,
		},
		{
			name:    "invalid base64 encoding in payload after signature forged check",
			token:   "v1.***bad***." + parts[2],
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketSignature,
		},
		{
			name:    "signed with wrong key",
			token:   wrongKeyToken,
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketSignature,
		},
		{
			name:    "tampered payload claims",
			token:   "v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin"}`)) + "." + parts[2],
			now:     time.Unix(1010, 0),
			wantErr: domain.ErrTicketSignature,
		},
		{
			name:    "expired ticket",
			token:   validToken,
			now:     time.Unix(1061, 0),
			wantErr: domain.ErrTicketExpired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := codec.Verify(tc.token, tc.now)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Verify(%q) err = %v, want %v", tc.token, err, tc.wantErr)
			}
		})
	}
}

func TestClaims_Validate(t *testing.T) {
	t.Parallel()

	secret := []byte("0123456789abcdef0123456789abcdef")
	codec, _ := ticket.NewCodec(secret)
	now := time.Unix(2000, 0)

	base := ticket.Claims{
		JTI:           "jti-123",
		Issuer:        "orbit",
		Subject:       "usr_42",
		WorkspaceID:   "ws_99",
		WorkspaceSlug: "acme-corp",
		UserLogin:     "david-s",
		Tier:          domain.TierStandard,
		IssuedAt:      1990,
		ExpiresAt:     2050,
	}

	mutate := func(fn func(*ticket.Claims)) ticket.Claims {
		c := base
		fn(&c)
		return c
	}

	cases := []struct {
		name    string
		claims  ticket.Claims
		wantErr error
	}{
		{
			name:    "empty JTI",
			claims:  mutate(func(c *ticket.Claims) { c.JTI = "" }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "wrong issuer",
			claims:  mutate(func(c *ticket.Claims) { c.Issuer = "other" }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "empty subject",
			claims:  mutate(func(c *ticket.Claims) { c.Subject = "" }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "invalid machine ID traversal",
			claims:  mutate(func(c *ticket.Claims) { c.WorkspaceID = "../bad" }),
			wantErr: domain.ErrInvalidMachineID,
		},
		{
			name:    "invalid workspace slug uppercase",
			claims:  mutate(func(c *ticket.Claims) { c.WorkspaceSlug = "Acme_Bad" }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "invalid user login",
			claims:  mutate(func(c *ticket.Claims) { c.UserLogin = "bad/user" }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "invalid tier",
			claims:  mutate(func(c *ticket.Claims) { c.Tier = "invalid" }),
			wantErr: domain.ErrInvalidResourceTier,
		},
		{
			name:    "exp before iat",
			claims:  mutate(func(c *ticket.Claims) { c.ExpiresAt = 1980 }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "lifetime exceeds max 120s",
			claims:  mutate(func(c *ticket.Claims) { c.ExpiresAt = 1990 + 121 }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
		{
			name:    "issued too far in the future",
			claims:  mutate(func(c *ticket.Claims) { c.IssuedAt = 2040; c.ExpiresAt = 2100 }),
			wantErr: domain.ErrTicketInvalidClaims,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, signErr := codec.Sign(tc.claims)
			rawToken := codec.SignUncheckedForTest(tc.claims)
			_, verifyErr := codec.Verify(rawToken, now)
			if !errors.Is(signErr, tc.wantErr) && !errors.Is(verifyErr, tc.wantErr) {
				t.Fatalf("Sign err = %v, Verify err = %v, want %v", signErr, verifyErr, tc.wantErr)
			}
		})
	}
}
