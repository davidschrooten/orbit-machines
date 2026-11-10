package storage_test

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/davidschrooten/orbit-machines/internal/domain"
	"github.com/davidschrooten/orbit-machines/internal/storage"
	"github.com/klauspost/compress/zstd"
)

func testAESKey() []byte {
	return []byte("0123456789abcdef0123456789abcdef") // 32 bytes
}

func TestProfileVault_SnapshotAndHydrateRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	memStore := storage.NewMemoryObjectStore()
	vault, err := storage.NewProfileVault(memStore, "orbit-user-profiles", testAESKey())
	if err != nil {
		t.Fatalf("NewProfileVault: %v", err)
	}

	mid, _ := domain.NewMachineID("ws_01HXYZ", "usr_01HABC")

	// 1. Hydrating when no profile exists returns (false, nil).
	emptyHome := t.TempDir()
	restored, err := vault.HydrateProfile(ctx, mid, emptyHome)
	if err != nil {
		t.Fatalf("HydrateProfile empty: %v", err)
	}
	if restored {
		t.Fatalf("expected restored=false on first boot")
	}

	// 2. Populate whitelisted and non-whitelisted files in srcHome.
	srcHome := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(srcHome, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	mustWrite(".ssh/id_ed25519", "PRIVATE_KEY_PERSISTED")
	mustWrite(".ssh/id_ed25519.pub", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI orbit@acme.dev\n")
	mustWrite(".gitconfig", "[user]\n\tname = Orbit\n")
	mustWrite(".config/orbit/state.json", `{"configured_tools":["git"]}`)
	mustWrite(".config/orbit-machine/settings.json", `{"theme":"catppuccin-mocha"}`)
	mustWrite(".claude/CLAUDE.md", "# Claude Preferences\n")
	mustWrite(".config/opencode/config.toml", "model = \"claude-sonnet\"\n")
	mustWrite(".gemini/settings.json", `{"mcp":true}`)
	mustWrite(".cache/untracked.tmp", "NOT_WHITELISTED")

	if err := vault.SnapshotProfile(ctx, mid, srcHome); err != nil {
		t.Fatalf("SnapshotProfile: %v", err)
	}

	// Verify encrypted blob is stored under mid.ProfileObjectKey() and does not leak plaintext.
	rawBlob, err := memStore.GetObject(ctx, "orbit-user-profiles", mid.ProfileObjectKey())
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if bytes.Contains(rawBlob, []byte("catppuccin-mocha")) || bytes.Contains(rawBlob, []byte("PRIVATE_KEY_PERSISTED")) {
		t.Fatalf("ciphertext leaked plaintext settings")
	}

	// 3. Hydrate into a fresh dstHome and verify whitelisted files are restored and non-whitelisted are excluded.
	dstHome := t.TempDir()
	restored, err = vault.HydrateProfile(ctx, mid, dstHome)
	if err != nil {
		t.Fatalf("HydrateProfile: %v", err)
	}
	if !restored {
		t.Fatalf("expected restored=true")
	}

	for rel, want := range map[string]string{
		".ssh/id_ed25519":                     "PRIVATE_KEY_PERSISTED",
		".ssh/id_ed25519.pub":                 "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI orbit@acme.dev\n",
		".gitconfig":                          "[user]\n\tname = Orbit\n",
		".config/orbit/state.json":            `{"configured_tools":["git"]}`,
		".config/orbit-machine/settings.json": `{"theme":"catppuccin-mocha"}`,
		".claude/CLAUDE.md":                   "# Claude Preferences\n",
		".config/opencode/config.toml":        "model = \"claude-sonnet\"\n",
		".gemini/settings.json":               `{"mcp":true}`,
	} {
		got, err := os.ReadFile(filepath.Join(dstHome, rel))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}

	if _, err := os.Stat(filepath.Join(dstHome, ".cache/untracked.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected non-whitelisted .cache/untracked.tmp to be excluded")
	}

	// 4. Cross-user AAD binding: another MachineID cannot decrypt mid's profile blob!
	otherMID, _ := domain.NewMachineID("ws_01HXYZ", "usr_attacker")
	_ = memStore.PutObject(ctx, "orbit-user-profiles", otherMID.ProfileObjectKey(), rawBlob)
	if _, err := vault.HydrateProfile(ctx, otherMID, t.TempDir()); !errors.Is(err, storage.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed on cross-user AAD mismatch, got %v", err)
	}

	// 5. Symlinked home directories & top-level files (as created by shellinit.EnsureInitialized) are followed by SnapshotProfile!
	symHome := filepath.Join(t.TempDir(), "sym-home")
	pvcHome := filepath.Join(t.TempDir(), "pvc-home")
	_ = os.MkdirAll(symHome, 0o755)
	_ = os.MkdirAll(filepath.Join(pvcHome, ".claude", "projects", "repo1"), 0o700)
	_ = os.MkdirAll(filepath.Join(pvcHome, ".gemini", "antigravity-cli", "conversations"), 0o700)
	_ = os.WriteFile(filepath.Join(pvcHome, ".claude", "projects", "repo1", "chat.jsonl"), []byte("{\"msg\":\"hi\"}\n"), 0o600)
	_ = os.WriteFile(filepath.Join(pvcHome, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"dev@acme.dev"}}`), 0o600)
	_ = os.WriteFile(filepath.Join(pvcHome, ".gemini", "antigravity-cli", "conversations", "c1.pb"), []byte("PB_DATA"), 0o600)
	_ = os.Symlink(filepath.Join(pvcHome, ".claude"), filepath.Join(symHome, ".claude"))
	_ = os.Symlink(filepath.Join(pvcHome, ".claude.json"), filepath.Join(symHome, ".claude.json"))
	_ = os.Symlink(filepath.Join(pvcHome, ".gemini"), filepath.Join(symHome, ".gemini"))

	symMID, _ := domain.NewMachineID("ws_sym", "usr_sym")
	if err := vault.SnapshotProfile(ctx, symMID, symHome); err != nil {
		t.Fatalf("SnapshotProfile(symHome): %v", err)
	}
	hydratedSymHome := t.TempDir()
	if ok, err := vault.HydrateProfile(ctx, symMID, hydratedSymHome); err != nil || !ok {
		t.Fatalf("HydrateProfile(symMID) = (%v, %v)", ok, err)
	}
	for rel, want := range map[string]string{
		".claude/projects/repo1/chat.jsonl":            "{\"msg\":\"hi\"}\n",
		".claude.json":                                 `{"oauthAccount":{"emailAddress":"dev@acme.dev"}}`,
		".gemini/antigravity-cli/conversations/c1.pb": "PB_DATA",
	} {
		got, err := os.ReadFile(filepath.Join(hydratedSymHome, rel))
		if err != nil || string(got) != want {
			t.Fatalf("hydrated symlinked %s = (%q, %v), want %q", rel, string(got), err, want)
		}
	}
}

func TestProfileVault_ZipSlipAndValidation(t *testing.T) {
	t.Parallel()

	memStore := storage.NewMemoryObjectStore()
	if _, err := storage.NewProfileVault(nil, "b", testAESKey()); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for nil store, got %v", err)
	}
	if _, err := storage.NewProfileVault(memStore, "", testAESKey()); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty bucket, got %v", err)
	}
	if _, err := storage.NewProfileVault(memStore, "b", []byte("short")); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for short key, got %v", err)
	}

	vault, _ := storage.NewProfileVault(memStore, "orbit-user-profiles", testAESKey())
	mid, _ := domain.NewMachineID("ws_1", "usr_1")

	// Craft a malicious tar.zst archive with a dir entry, a zero-mode entry, and "../escape.txt".
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	_ = tw.WriteHeader(&tar.Header{Name: ".claude/", Typeflag: tar.TypeDir})
	_ = tw.WriteHeader(&tar.Header{Name: ".claude/zero.txt", Mode: 0, Size: 2, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("ok"))
	evilPayload := []byte("pwned")
	_ = tw.WriteHeader(&tar.Header{
		Name:     "../escape.txt",
		Mode:     0o600,
		Size:     int64(len(evilPayload)),
		Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write(evilPayload)
	_ = tw.Close()

	enc, _ := zstd.NewWriter(nil)
	compressed := enc.EncodeAll(tarBuf.Bytes(), nil)
	ciphertext, err := vault.EncryptBlob(compressed, mid.ProfileObjectKey())
	if err != nil {
		t.Fatalf("EncryptBlob: %v", err)
	}
	_ = memStore.PutObject(context.Background(), "orbit-user-profiles", mid.ProfileObjectKey(), ciphertext)

	if _, err := vault.HydrateProfile(context.Background(), mid, t.TempDir()); !errors.Is(err, storage.ErrUnsafeArchiveEntry) {
		t.Fatalf("expected ErrUnsafeArchiveEntry on zip-slip tar entry, got %v", err)
	}
}

func TestHTTPObjectStore_PutGetListDelete(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		objects = make(map[string][]byte)
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		path := strings.TrimPrefix(r.URL.Path, "/")
		parts := strings.SplitN(path, "/", 2)
		bucket := parts[0]

		if r.Method == http.MethodGet && len(parts) == 1 {
			prefix := r.URL.Query().Get("prefix")
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`))
			for k := range objects {
				bPrefix := bucket + "/"
				if strings.HasPrefix(k, bPrefix) {
					objKey := strings.TrimPrefix(k, bPrefix)
					if strings.HasPrefix(objKey, prefix) {
						_, _ = w.Write([]byte("<Contents><Key>" + objKey + "</Key></Contents>"))
					}
				}
			}
			_, _ = w.Write([]byte(`</ListBucketResult>`))
			return
		}

		if len(parts) < 2 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		fullKey := bucket + "/" + parts[1]

		switch r.Method {
		case http.MethodPut:
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(r.Body)
			objects[fullKey] = buf.Bytes()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			data, ok := objects[fullKey]
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		case http.MethodDelete:
			delete(objects, fullKey)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	if _, err := storage.NewHTTPObjectStore("", nil); !errors.Is(err, domain.ErrInvalidConfig) {
		t.Fatalf("expected ErrInvalidConfig for empty endpoint, got %v", err)
	}

	store, err := storage.NewHTTPObjectStore(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("NewHTTPObjectStore: %v", err)
	}

	ctx := context.Background()
	if _, err := store.GetObject(ctx, "orbit-user-profiles", "missing"); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound, got %v", err)
	}

	if err := store.PutObject(ctx, "orbit-user-profiles", "profiles/ws_1/usr_1/a.bin", []byte("hello")); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	got, err := store.GetObject(ctx, "orbit-user-profiles", "profiles/ws_1/usr_1/a.bin")
	if err != nil || string(got) != "hello" {
		t.Fatalf("GetObject = %q, err=%v", got, err)
	}

	keys, err := store.ListObjects(ctx, "orbit-user-profiles", "profiles/ws_1/")
	if err != nil || len(keys) != 1 || keys[0] != "profiles/ws_1/usr_1/a.bin" {
		t.Fatalf("ListObjects = %v, err=%v", keys, err)
	}

	if err := store.DeleteObject(ctx, "orbit-user-profiles", "profiles/ws_1/usr_1/a.bin"); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := store.GetObject(ctx, "orbit-user-profiles", "profiles/ws_1/usr_1/a.bin"); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound after DeleteObject, got %v", err)
	}
}

func TestStorage_MemoryStoreAndErrorPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	mem := storage.NewMemoryObjectStore()
	_ = mem.PutObject(ctx, "b1", "prefix/b", []byte("2"))
	_ = mem.PutObject(ctx, "b1", "prefix/a", []byte("1"))
	_ = mem.PutObject(ctx, "b2", "prefix/c", []byte("3"))

	keys, err := mem.ListObjects(ctx, "b1", "prefix/")
	if err != nil || len(keys) != 2 || keys[0] != "prefix/a" || keys[1] != "prefix/b" {
		t.Fatalf("unexpected MemoryObjectStore.ListObjects: %v, err=%v", keys, err)
	}
	if err := mem.DeleteObject(ctx, "b1", "prefix/a"); err != nil {
		t.Fatalf("MemoryObjectStore.DeleteObject: %v", err)
	}
	if _, err := mem.GetObject(ctx, "b1", "prefix/a"); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("expected ErrObjectNotFound after delete")
	}

	vault, _ := storage.NewProfileVault(mem, "b1", testAESKey())
	if vault.Store() != mem || vault.Bucket() != "b1" {
		t.Fatalf("unexpected Store/Bucket accessors")
	}
	if _, err := vault.DecryptBlob([]byte("short"), "aad"); !errors.Is(err, storage.ErrDecryptionFailed) {
		t.Fatalf("expected ErrDecryptionFailed on short blob, got %v", err)
	}

	// Non-zstd payload inside valid ciphertext triggers decompression error in HydrateProfile.
	mid, _ := domain.NewMachineID("ws_corrupt", "usr_corrupt")
	badZstdCipher, _ := vault.EncryptBlob([]byte("not-valid-zstd-stream"), mid.ProfileObjectKey())
	_ = mem.PutObject(ctx, "b1", mid.ProfileObjectKey(), badZstdCipher)
	if _, err := vault.HydrateProfile(ctx, mid, t.TempDir()); err == nil {
		t.Fatalf("expected error on corrupt zstd stream in HydrateProfile")
	}

	// HTTPObjectStore 500 error branches, invalid XML, cancelled context, and 404 delete idempotency.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("prefix") == "badxml" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("<<invalid-xml"))
			return
		}
		http.Error(w, "s3 error", http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	httpStore, err := storage.NewHTTPObjectStore(errSrv.URL, nil)
	if err != nil {
		t.Fatalf("NewHTTPObjectStore: %v", err)
	}
	if err := httpStore.DeleteObject(ctx, "b", "missing"); err != nil {
		t.Fatalf("expected 404 DeleteObject to succeed idempotently, got %v", err)
	}
	if _, err := httpStore.GetObject(ctx, "b", "k"); err == nil {
		t.Fatalf("expected GetObject error on 500")
	}
	if err := httpStore.PutObject(ctx, "b", "k", []byte("x")); err == nil {
		t.Fatalf("expected PutObject error on 500")
	}
	if err := httpStore.DeleteObject(ctx, "b", "k"); err == nil {
		t.Fatalf("expected DeleteObject error on 500")
	}
	if _, err := httpStore.ListObjects(ctx, "b", "p"); err == nil {
		t.Fatalf("expected ListObjects error on 500")
	}
	if _, err := httpStore.ListObjects(ctx, "b", "badxml"); err == nil {
		t.Fatalf("expected ListObjects error on malformed XML")
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = httpStore.GetObject(canceledCtx, "b", "k")
	_ = httpStore.PutObject(canceledCtx, "b", "k", []byte("x"))
	_ = httpStore.DeleteObject(canceledCtx, "b", "k")
	_, _ = httpStore.ListObjects(canceledCtx, "b", "p")
}
