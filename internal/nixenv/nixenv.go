// Package nixenv implements Nix devshell environment parsing, project stack
// detection, flake.nix scaffolding, and post-build binary cache uploading.
package nixenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/storage"
)

// ErrInvalidStorePath is returned when a store path does not reside under /nix/store/.
var ErrInvalidStorePath = errors.New("invalid nix store path")

type devEnvDocument struct {
	Variables map[string]struct {
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	} `json:"variables"`
}

// ParseDevEnvJSON extracts exported string environment variables from the output
// of `nix print-dev-env --json`.
func ParseDevEnvJSON(raw []byte) (map[string]string, error) {
	var doc devEnvDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing nix print-dev-env json: %w", err)
	}

	out := make(map[string]string, len(doc.Variables))
	for name, entry := range doc.Variables {
		if entry.Type != "exported" {
			continue
		}
		var strVal string
		if err := json.Unmarshal(entry.Value, &strVal); err != nil {
			continue
		}
		out[name] = strVal
	}
	return out, nil
}

// MergeEnv merges exported Nix devshell variables onto a base KEY=VALUE slice,
// prepending overrides["PATH"] onto any existing PATH entry.
func MergeEnv(base []string, overrides map[string]string) []string {
	merged := make(map[string]string, len(base)+len(overrides))
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			merged[k] = v
		}
	}
	for k, v := range overrides {
		if k == "PATH" {
			if existing, ok := merged["PATH"]; ok && existing != "" {
				merged["PATH"] = v + ":" + existing
				continue
			}
		}
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

// HasFlake reports whether repoDir contains a flake.nix file.
func HasFlake(repoDir string) bool {
	info, err := os.Stat(filepath.Join(repoDir, "flake.nix"))
	return err == nil && !info.IsDir()
}

// DetectStacks inspects marker files in repoDir and returns a sorted list of detected ecosystems.
func DetectStacks(repoDir string) ([]string, error) {
	markers := map[string]string{
		"go.mod":           "go",
		"Gemfile":          "ruby",
		"package.json":     "nodejs",
		"Cargo.toml":       "rust",
		"pyproject.toml":   "python",
		"requirements.txt": "python",
		"flake.nix":        "nix",
	}

	seen := make(map[string]struct{})
	for file, stack := range markers {
		if _, err := os.Stat(filepath.Join(repoDir, file)); err == nil {
			seen[stack] = struct{}{}
		}
	}

	stacks := make([]string, 0, len(seen))
	for s := range seen {
		stacks = append(stacks, s)
	}
	sort.Strings(stacks)
	return stacks, nil
}

// GenerateFlakeNix renders a multi-system flake.nix with a mkShell devShell containing packages.
func GenerateFlakeNix(description string, packages []string) string {
	desc := strings.TrimSpace(description)
	if desc == "" {
		desc = "Orbit workspace development environment"
	}

	var pkgLines strings.Builder
	for _, pkg := range packages {
		trimmed := strings.TrimSpace(pkg)
		if trimmed != "" {
			pkgLines.WriteString("            pkgs." + trimmed + "\n")
		}
	}

	return fmt.Sprintf(`{
  description = %q;

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
    in
    {
      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
%s            ];
          };
        });
    };
}
`, desc, pkgLines.String())
}

// CacheUploader uploads newly built /nix/store paths to the cluster's S3/MinIO Nix binary cache.
type CacheUploader struct {
	store  storage.ObjectStore
	bucket string
}

// NewCacheUploader validates store and bucket and returns a CacheUploader.
func NewCacheUploader(store storage.ObjectStore, bucket string) (*CacheUploader, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: ObjectStore is required", domain.ErrInvalidConfig)
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("%w: nix cache bucket is required", domain.ErrInvalidConfig)
	}
	return &CacheUploader{
		store:  store,
		bucket: strings.TrimSpace(bucket),
	}, nil
}

// UploadStorePaths validates each store path and records its .narinfo entry in the S3 binary cache bucket.
func (u *CacheUploader) UploadStorePaths(ctx context.Context, paths []string) (int, error) {
	uploaded := 0
	for _, rawPath := range paths {
		clean := filepath.Clean(strings.TrimSpace(rawPath))
		if !strings.HasPrefix(clean, "/nix/store/") {
			return uploaded, fmt.Errorf("%w: %q", ErrInvalidStorePath, rawPath)
		}
		base := filepath.Base(clean)
		hashPart, _, ok := strings.Cut(base, "-")
		if !ok || len(hashPart) < 8 {
			return uploaded, fmt.Errorf("%w: missing store hash prefix in %q", ErrInvalidStorePath, rawPath)
		}

		narInfo := fmt.Sprintf("StorePath: %s\nCompression: zstd\n", clean)
		key := hashPart + ".narinfo"
		if err := u.store.PutObject(ctx, u.bucket, key, []byte(narInfo)); err != nil {
			return uploaded, err
		}
		uploaded++
	}
	return uploaded, nil
}
