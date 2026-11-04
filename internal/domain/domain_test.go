package domain_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
)

var rfc1123LabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func TestNewMachineID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		workspaceID string
		userID      string
		wantPod     string
		wantProfile string
		wantErr     error
	}{
		{
			name:        "valid standard IDs",
			workspaceID: "ws_99",
			userID:      "usr_42",
			wantPod:     "machine-ws-99-usr-42",
			wantProfile: "profiles/ws_99/usr_42/profile.tar.zst.enc",
		},
		{
			name:        "valid uppercase and mixed characters sanitized into DNS label",
			workspaceID: "WS_Acme.Prod",
			userID:      "User_David@Example",
			wantProfile: "profiles/WS_Acme.Prod/User_David@Example/profile.tar.zst.enc",
		},
		{
			name:        "leading hyphens and punctuation fallback to safe fragment",
			workspaceID: "---",
			userID:      "___usr__42",
			wantProfile: "profiles/---/___usr__42/profile.tar.zst.enc",
		},
		{
			name:        "empty workspace ID rejected",
			workspaceID: "   ",
			userID:      "usr_42",
			wantErr:     domain.ErrInvalidMachineID,
		},
		{
			name:        "empty user ID rejected",
			workspaceID: "ws_99",
			userID:      "",
			wantErr:     domain.ErrInvalidMachineID,
		},
		{
			name:        "path traversal in workspace ID rejected",
			workspaceID: "../etc",
			userID:      "usr_42",
			wantErr:     domain.ErrInvalidMachineID,
		},
		{
			name:        "slash in user ID rejected",
			workspaceID: "ws_99",
			userID:      "usr/42",
			wantErr:     domain.ErrInvalidMachineID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id, err := domain.NewMachineID(tc.workspaceID, tc.userID)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("NewMachineID(%q, %q) err = %v, want %v", tc.workspaceID, tc.userID, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewMachineID(%q, %q) unexpected error: %v", tc.workspaceID, tc.userID, err)
			}

			pod := id.PodName()
			if len(pod) > 63 || !rfc1123LabelRe.MatchString(pod) {
				t.Fatalf("PodName() = %q, not a valid RFC 1123 label <= 63 chars", pod)
			}
			if tc.wantPod != "" && pod != tc.wantPod {
				t.Errorf("PodName() = %q, want %q", pod, tc.wantPod)
			}
			if got := id.ProfileObjectKey(); got != tc.wantProfile {
				t.Errorf("ProfileObjectKey() = %q, want %q", got, tc.wantProfile)
			}
			if got := id.WorkspaceID(); got != strings.TrimSpace(tc.workspaceID) {
				t.Errorf("WorkspaceID() = %q, want %q", got, strings.TrimSpace(tc.workspaceID))
			}
			if got := id.UserID(); got != strings.TrimSpace(tc.userID) {
				t.Errorf("UserID() = %q, want %q", got, strings.TrimSpace(tc.userID))
			}
			wantWIP := "profiles/" + id.WorkspaceID() + "/" + id.UserID() + "/wip/repo_1.bundle.tar.zst.enc"
			if got := id.WIPBundleObjectKey("repo_1"); got != wantWIP {
				t.Errorf("WIPBundleObjectKey() = %q, want %q", got, wantWIP)
			}
		})
	}
}

func TestMachineID_LongNamesRemainUniqueAndBounded(t *testing.T) {
	t.Parallel()

	longWS := "workspace-" + strings.Repeat("a", 50)
	id1, err := domain.NewMachineID(longWS, "user-alpha-1")
	if err != nil {
		t.Fatalf("NewMachineID id1 unexpected error: %v", err)
	}
	id2, err := domain.NewMachineID(longWS, "user-alpha-2")
	if err != nil {
		t.Fatalf("NewMachineID id2 unexpected error: %v", err)
	}

	pod1 := id1.PodName()
	pod2 := id2.PodName()
	if len(pod1) > 63 || !rfc1123LabelRe.MatchString(pod1) {
		t.Fatalf("pod1 = %q (len %d), invalid RFC 1123 label", pod1, len(pod1))
	}
	if pod1 == pod2 {
		t.Fatalf("expected distinct pod names for distinct long IDs, got identical %q", pod1)
	}
}

func TestParseResourceTier(t *testing.T) {
	t.Parallel()

	std, err := domain.ParseResourceTier("standard")
	if err != nil {
		t.Fatalf("ParseResourceTier(standard) error: %v", err)
	}
	if std != domain.TierStandard {
		t.Fatalf("got %q, want %q", std, domain.TierStandard)
	}
	stdSpecs := std.Specs()
	if stdSpecs.CPURequest != "1000m" || stdSpecs.CPULimit != "2000m" || stdSpecs.MemoryLimit != "4Gi" {
		t.Errorf("unexpected standard specs: %+v", stdSpecs)
	}

	perf, err := domain.ParseResourceTier("performance")
	if err != nil {
		t.Fatalf("ParseResourceTier(performance) error: %v", err)
	}
	perfSpecs := perf.Specs()
	if perfSpecs.CPURequest != "2000m" || perfSpecs.CPULimit != "4000m" || perfSpecs.MemoryLimit != "8Gi" {
		t.Errorf("unexpected performance specs: %+v", perfSpecs)
	}

	if _, err := domain.ParseResourceTier("ultra"); !errors.Is(err, domain.ErrInvalidResourceTier) {
		t.Fatalf("ParseResourceTier(ultra) err = %v, want %v", err, domain.ErrInvalidResourceTier)
	}
}
