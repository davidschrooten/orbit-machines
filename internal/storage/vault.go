// Package storage provides S3/MinIO object storage and AES-256-GCM + zstd
// encrypted user profile archives for Orbit Machines.
package storage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/klauspost/compress/zstd"
)

// Sentinel errors for object storage and encrypted archives.
var (
	ErrObjectNotFound     = errors.New("s3 object not found")
	ErrDecryptionFailed   = errors.New("profile archive decryption failed")
	ErrUnsafeArchiveEntry = errors.New("archive entry attempts path traversal")
)

const magicHeader = "ORBM"

// DefaultProfileWhitelist lists user home relative paths synced across ephemeral Pods.
var DefaultProfileWhitelist = []string{
	".ssh",
	".gitconfig",
	".config/orbit",
	".config/orbit-machine",
	".config/gh",
	".config/antigravity",
	".claude",
	".claude.json",
	".config/opencode",
	".gemini",
}

// ObjectStore abstracts S3/MinIO bucket operations.
type ObjectStore interface {
	GetObject(ctx context.Context, bucket, key string) ([]byte, error)
	PutObject(ctx context.Context, bucket, key string, data []byte) error
	DeleteObject(ctx context.Context, bucket, key string) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]string, error)
}

// MemoryObjectStore is an in-memory ObjectStore for unit and integration tests.
type MemoryObjectStore struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

// NewMemoryObjectStore creates an empty MemoryObjectStore.
func NewMemoryObjectStore() *MemoryObjectStore {
	return &MemoryObjectStore{
		objects: make(map[string][]byte),
	}
}

// GetObject retrieves an object by bucket and key.
func (m *MemoryObjectStore) GetObject(_ context.Context, bucket, key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, ok := m.objects[bucket+"/"+key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out, nil
}

// PutObject stores a copy of data under bucket and key.
func (m *MemoryObjectStore) PutObject(_ context.Context, bucket, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, len(data))
	copy(buf, data)
	m.objects[bucket+"/"+key] = buf
	return nil
}

// DeleteObject removes an object by bucket and key.
func (m *MemoryObjectStore) DeleteObject(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, bucket+"/"+key)
	return nil
}

// ListObjects returns all object keys in bucket matching prefix in sorted order.
func (m *MemoryObjectStore) ListObjects(_ context.Context, bucket, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	bPrefix := bucket + "/"
	var keys []string
	for fullKey := range m.objects {
		if strings.HasPrefix(fullKey, bPrefix) {
			key := strings.TrimPrefix(fullKey, bPrefix)
			if strings.HasPrefix(key, prefix) {
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// HTTPObjectStore is a path-style S3/MinIO HTTP client.
type HTTPObjectStore struct {
	endpoint string
	client   *http.Client
}

// NewHTTPObjectStore validates endpoint and constructs an HTTPObjectStore.
func NewHTTPObjectStore(endpoint string, client *http.Client) (*HTTPObjectStore, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%w: invalid s3 endpoint %q", domain.ErrInvalidConfig, endpoint)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPObjectStore{
		endpoint: trimmed,
		client:   client,
	}, nil
}

// GetObject fetches an object over HTTP GET /{bucket}/{key}.
func (s *HTTPObjectStore) GetObject(ctx context.Context, bucket, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.objectURL(bucket, key), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrObjectNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 GET %s/%s returned status %d", bucket, key, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// PutObject uploads data over HTTP PUT /{bucket}/{key}.
func (s *HTTPObjectStore) PutObject(ctx context.Context, bucket, key string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(bucket, key), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("s3 PUT %s/%s returned status %d", bucket, key, resp.StatusCode)
	}
	return nil
}

// DeleteObject deletes an object over HTTP DELETE /{bucket}/{key}.
func (s *HTTPObjectStore) DeleteObject(ctx context.Context, bucket, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.objectURL(bucket, key), nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("s3 DELETE %s/%s returned status %d", bucket, key, resp.StatusCode)
	}
	return nil
}

type listBucketResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
}

// ListObjects lists object keys matching prefix via GET /{bucket}?list-type=2&prefix={prefix}.
func (s *HTTPObjectStore) ListObjects(ctx context.Context, bucket, prefix string) ([]string, error) {
	u := fmt.Sprintf("%s/%s?list-type=2&prefix=%s", s.endpoint, url.PathEscape(bucket), url.QueryEscape(prefix))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 LIST %s returned status %d", bucket, resp.StatusCode)
	}
	var parsed listBucketResult
	if err := xml.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding s3 list xml: %w", err)
	}
	keys := make([]string, 0, len(parsed.Contents))
	for _, item := range parsed.Contents {
		keys = append(keys, item.Key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *HTTPObjectStore) objectURL(bucket, key string) string {
	return fmt.Sprintf("%s/%s/%s", s.endpoint, strings.TrimPrefix(bucket, "/"), strings.TrimPrefix(key, "/"))
}

// ProfileVault encrypts, compresses, and syncs whitelisted user profile directories
// (and arbitrary encrypted blobs) to S3/MinIO using AES-256-GCM and zstd.
type ProfileVault struct {
	store   ObjectStore
	bucket  string
	aead    cipher.AEAD
	encoder *zstd.Encoder
	decoder *zstd.Decoder
}

// NewProfileVault validates dependencies and constructs a ProfileVault with a 32-byte AES-256 key.
func NewProfileVault(store ObjectStore, bucket string, key []byte) (*ProfileVault, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: ObjectStore is required", domain.ErrInvalidConfig)
	}
	if strings.TrimSpace(bucket) == "" {
		return nil, fmt.Errorf("%w: profile bucket is required", domain.ErrInvalidConfig)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%w: AES-256 key must be 32 bytes, got %d", domain.ErrInvalidConfig, len(key))
	}

	block, _ := aes.NewCipher(key)
	aead, _ := cipher.NewGCM(block)
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	dec, _ := zstd.NewReader(nil)

	return &ProfileVault{
		store:   store,
		bucket:  strings.TrimSpace(bucket),
		aead:    aead,
		encoder: enc,
		decoder: dec,
	}, nil
}

// Store returns the underlying ObjectStore.
func (v *ProfileVault) Store() ObjectStore { return v.store }

// Bucket returns the configured S3 bucket name.
func (v *ProfileVault) Bucket() string { return v.bucket }

// EncryptBlob encrypts plaintext with AES-256-GCM binding aad as authenticated associated data.
func (v *ProfileVault) EncryptBlob(plaintext []byte, aad string) ([]byte, error) {
	nonce := make([]byte, v.aead.NonceSize())
	_, _ = io.ReadFull(rand.Reader, nonce)
	header := append([]byte(magicHeader), nonce...)
	return v.aead.Seal(header, nonce, plaintext, []byte(aad)), nil
}

// DecryptBlob verifies the magic header and decrypts ciphertext with AES-256-GCM and aad.
func (v *ProfileVault) DecryptBlob(ciphertext []byte, aad string) ([]byte, error) {
	minLen := len(magicHeader) + v.aead.NonceSize() + v.aead.Overhead()
	if len(ciphertext) < minLen || string(ciphertext[:len(magicHeader)]) != magicHeader {
		return nil, ErrDecryptionFailed
	}
	nonceStart := len(magicHeader)
	nonceEnd := nonceStart + v.aead.NonceSize()
	nonce := ciphertext[nonceStart:nonceEnd]
	sealed := ciphertext[nonceEnd:]

	plain, err := v.aead.Open(nil, nonce, sealed, []byte(aad))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecryptionFailed, err)
	}
	return plain, nil
}

// SnapshotProfile archives, zstd-compresses, and AES-256-GCM-encrypts whitelisted paths
// under homeDir and uploads the blob to mid.ProfileObjectKey().
func (v *ProfileVault) SnapshotProfile(ctx context.Context, mid domain.MachineID, homeDir string) error {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)

	for _, relRoot := range DefaultProfileWhitelist {
		absRoot := filepath.Join(homeDir, relRoot)
		if _, err := os.Stat(absRoot); errors.Is(err, os.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() || !d.Type().IsRegular() {
				return walkErr
			}
			relPath, _ := filepath.Rel(homeDir, path)
			info, _ := d.Info()
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_ = tw.WriteHeader(&tar.Header{
				Name:     filepath.ToSlash(relPath),
				Mode:     int64(info.Mode().Perm()),
				Size:     int64(len(data)),
				ModTime:  info.ModTime(),
				Typeflag: tar.TypeReg,
			})
			_, _ = tw.Write(data)
			return nil
		})
		if err != nil {
			return err
		}
	}
	_ = tw.Close()

	compressed := v.encoder.EncodeAll(tarBuf.Bytes(), nil)
	objectKey := mid.ProfileObjectKey()
	encrypted, _ := v.EncryptBlob(compressed, objectKey)
	return v.store.PutObject(ctx, v.bucket, objectKey, encrypted)
}

// ResolveSafeExtractPath validates that entryName resolves strictly inside baseDir.
func ResolveSafeExtractPath(baseDir, entryName string) (string, error) {
	cleanBase := filepath.Clean(baseDir)
	cleanRel := filepath.Clean(filepath.FromSlash(entryName))
	if cleanRel == "." || filepath.IsAbs(cleanRel) || strings.HasPrefix(cleanRel, "..") {
		return "", fmt.Errorf("%w: %q", ErrUnsafeArchiveEntry, entryName)
	}
	return filepath.Join(cleanBase, cleanRel), nil
}

// HydrateProfile downloads, decrypts, decompresses, and extracts a user's profile archive into homeDir.
// Returns (false, nil) when no profile archive exists yet in S3.
func (v *ProfileVault) HydrateProfile(ctx context.Context, mid domain.MachineID, homeDir string) (bool, error) {
	objectKey := mid.ProfileObjectKey()
	encrypted, err := v.store.GetObject(ctx, v.bucket, objectKey)
	if errors.Is(err, ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	compressed, err := v.DecryptBlob(encrypted, objectKey)
	if err != nil {
		return false, err
	}

	tarBytes, err := v.decoder.DecodeAll(compressed, nil)
	if err != nil {
		return false, fmt.Errorf("decompressing profile archive: %w", err)
	}

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

		targetPath, err := ResolveSafeExtractPath(homeDir, hdr.Name)
		if err != nil {
			return false, err
		}
		_ = os.MkdirAll(filepath.Dir(targetPath), 0o755)
		if strings.HasPrefix(filepath.ToSlash(hdr.Name), ".ssh/") {
			_ = os.Chmod(filepath.Join(homeDir, ".ssh"), 0o700)
		}
		content, _ := io.ReadAll(io.LimitReader(tr, 64<<20))
		mode := fs.FileMode(hdr.Mode) & 0o777
		if mode == 0 {
			mode = 0o600
		}
		if err := os.WriteFile(targetPath, content, mode); err != nil {
			return false, err
		}
		_ = os.Chmod(targetPath, mode)
	}
	return true, nil
}
