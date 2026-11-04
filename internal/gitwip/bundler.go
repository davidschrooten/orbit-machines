// Package gitwip snapshots and restores uncommitted Git working trees and local
// branch commits across idle Pod evictions using encrypted git bundles in S3.
package gitwip

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/storage"
	"github.com/klauspost/compress/zstd"
)

const (
	bundleEntryName = ".orbit-wip/repo.bundle"
	headEntryName   = ".orbit-wip/head-branch"
	filesPrefix     = "worktree/"
)

// Bundler detects dirty git repositories, snapshots `git bundle` + uncommitted
// working tree files to encrypted S3 objects, and restores them on next boot.
type Bundler struct {
	vault   *storage.ProfileVault
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// NewBundler validates vault and returns a Bundler.
func NewBundler(vault *storage.ProfileVault) (*Bundler, error) {
	if vault == nil {
		return nil, fmt.Errorf("%w: ProfileVault is required", domain.ErrInvalidConfig)
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)
	return &Bundler{
		vault:   vault,
		encoder: enc,
		decoder: dec,
	}, nil
}

// IsDirty reports whether repoDir has uncommitted changes or untracked files.
func (b *Bundler) IsDirty(ctx context.Context, repoDir string) (bool, error) {
	out, err := runGitOutput(ctx, repoDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// HasPendingBundle checks whether an encrypted WIP bundle exists in S3 for (mid, repoID).
func (b *Bundler) HasPendingBundle(ctx context.Context, mid domain.MachineID, repoID string) (bool, error) {
	key := mid.WIPBundleObjectKey(repoID)
	_, err := b.vault.Store().GetObject(ctx, b.vault.Bucket(), key)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// SnapshotRepo creates an encrypted WIP bundle in S3 if repoDir is dirty.
// Returns (false, nil) when the repository is clean.
func (b *Bundler) SnapshotRepo(ctx context.Context, mid domain.MachineID, repoID, repoDir string) (bool, error) {
	dirty, err := b.IsDirty(ctx, repoDir)
	if err != nil || !dirty {
		return false, err
	}

	tmpDir, _ := os.MkdirTemp("", "orbit-wip-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)

	bundlePath := filepath.Join(tmpDir, "repo.bundle")
	if _, bundleErr := runGitOutput(ctx, repoDir, "bundle", "create", bundlePath, "--all"); bundleErr == nil {
		if bundleBytes, readErr := os.ReadFile(bundlePath); readErr == nil {
			writeTarEntry(tw, bundleEntryName, 0o600, bundleBytes)
		}
	}

	if branchOut, brErr := runGitOutput(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD"); brErr == nil {
		branch := strings.TrimSpace(string(branchOut))
		if branch != "" && branch != "HEAD" {
			writeTarEntry(tw, headEntryName, 0o600, []byte(branch))
		}
	}

	err = filepath.WalkDir(repoDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(repoDir, path)
		info, _ := d.Info()
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		writeTarEntry(tw, filesPrefix+filepath.ToSlash(rel), int64(info.Mode().Perm()), data)
		return nil
	})
	if err != nil {
		return false, err
	}
	_ = tw.Close()

	compressed := b.encoder.EncodeAll(tarBuf.Bytes(), nil)
	objectKey := mid.WIPBundleObjectKey(repoID)
	encrypted, _ := b.vault.EncryptBlob(compressed, objectKey)
	if err := b.vault.Store().PutObject(ctx, b.vault.Bucket(), objectKey, encrypted); err != nil {
		return false, err
	}
	return true, nil
}

// RestoreRepo downloads, decrypts, unbundles git refs, and extracts dirty working tree
// files into repoDir, then deletes the consumed bundle from S3.
func (b *Bundler) RestoreRepo(ctx context.Context, mid domain.MachineID, repoID, repoDir string) (bool, error) {
	objectKey := mid.WIPBundleObjectKey(repoID)
	encrypted, err := b.vault.Store().GetObject(ctx, b.vault.Bucket(), objectKey)
	if errors.Is(err, storage.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	compressed, err := b.vault.DecryptBlob(encrypted, objectKey)
	if err != nil {
		return false, err
	}
	tarBytes, err := b.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return false, fmt.Errorf("decompressing wip bundle: %w", err)
	}

	tmpDir, _ := os.MkdirTemp("", "orbit-wip-restore-*")
	defer func() { _ = os.RemoveAll(tmpDir) }()

	var (
		bundleBytes []byte
		headBranch  string
		files       = make(map[string]extractedFile)
	)

	tr := tar.NewReader(bytes.NewReader(tarBytes))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		content, _ := io.ReadAll(io.LimitReader(tr, 64<<20))

		switch {
		case hdr.Name == bundleEntryName:
			bundleBytes = content
		case hdr.Name == headEntryName:
			headBranch = strings.TrimSpace(string(content))
		case strings.HasPrefix(hdr.Name, filesPrefix):
			rel := strings.TrimPrefix(hdr.Name, filesPrefix)
			targetPath, err := storage.ResolveSafeExtractPath(repoDir, rel)
			if err != nil {
				return false, err
			}
			mode := fs.FileMode(hdr.Mode) & 0o777
			if mode == 0 {
				mode = 0o644
			}
			files[targetPath] = extractedFile{data: content, mode: mode}
		}
	}

	if len(bundleBytes) > 0 {
		bundlePath := filepath.Join(tmpDir, "repo.bundle")
		_ = os.WriteFile(bundlePath, bundleBytes, 0o600)
		_, _ = runGitOutput(ctx, repoDir, "fetch", bundlePath, "refs/heads/*:refs/heads/*")
		if headBranch != "" {
			_, _ = runGitOutput(ctx, repoDir, "checkout", "-f", headBranch)
		}
	}

	for targetPath, f := range files {
		_ = os.MkdirAll(filepath.Dir(targetPath), 0o755)
		if err := os.WriteFile(targetPath, f.data, f.mode); err != nil {
			return false, err
		}
	}

	_ = b.vault.Store().DeleteObject(ctx, b.vault.Bucket(), objectKey)
	return true, nil
}

type extractedFile struct {
	data []byte
	mode fs.FileMode
}

func writeTarEntry(tw *tar.Writer, name string, mode int64, data []byte) {
	_ = tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
		Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write(data)
}

func runGitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w (%s)", args, err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
