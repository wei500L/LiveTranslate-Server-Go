package modelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureModel builds a catalog-shaped model backed by deterministic
// bytes, so download/verify paths can be exercised without multi-GB
// files. The Downloader.Source override points it at an httptest server.
func fixtureModel(t *testing.T, body string) (Model, []byte) {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	return Model{
		ID:       "test-model",
		File:     "test.bin",
		Bytes:    int64(len(body)),
		SHA256:   hex.EncodeToString(sum[:]),
		Upstream: "https://example.invalid/test.bin",
		Title:    "fixture",
	}, []byte(body)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreFilePathRejectsUnknownAndTraversal(t *testing.T) {
	s := newTestStore(t)

	if _, err := s.FilePath("hy-mt2-1.8b-q4km", "Hy-MT2-1.8B-Q4_K_M.gguf"); err != nil {
		t.Fatalf("catalog model should resolve even when absent on disk: %v", err)
	}
	// Unknown id.
	if _, err := s.FilePath("no-such-model", "x.bin"); err == nil {
		t.Fatal("unknown id must be rejected")
	}
	// Known id but wrong filename — including traversal attempts: the
	// file component must match the CATALOG name exactly, so nothing
	// client-supplied can ever steer the path.
	for _, bad := range []string{"../root", "Hy-MT2-1.8B-Q4_K_M.gguf/", "other.gguf", "."} {
		if _, err := s.FilePath("hy-mt2-1.8b-q4km", bad); err == nil {
			t.Fatalf("file %q must be rejected", bad)
		}
	}
}

func TestDownloaderVerifiesAndIsIdempotent(t *testing.T) {
	m, body := fixtureModel(t, "hello model bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, string(body))
	}))
	defer srv.Close()

	store := newTestStore(t)
	dl := Downloader{Source: func(Model) string { return srv.URL }}
	// The catalog is package-level; drive downloadOne directly against
	// the fixture model.
	if err := dl.downloadOne(store, m); err != nil {
		t.Fatalf("download: %v", err)
	}
	if !store.Installed(m) {
		t.Fatal("model should be installed after download")
	}
	if err := store.Verify(m); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// No .partial leftovers.
	if _, err := os.Stat(filepath.Join(store.Root(), m.ID, m.File+".partial")); !os.IsNotExist(err) {
		t.Fatal(".partial must be renamed away")
	}
}

func TestDownloaderRejectsCorruptBytes(t *testing.T) {
	m, body := fixtureModel(t, "good bytes")
	// Same length, different content: the size gate passes, so the
	// SHA256 gate is the one that must catch it.
	bad := append([]byte(nil), body...)
	bad[0] ^= 0xff
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bad)
	}))
	defer srv.Close()

	store := newTestStore(t)
	dl := Downloader{Source: func(Model) string { return srv.URL }}
	err := dl.downloadOne(store, m)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("corrupt bytes must fail the hash gate, got: %v", err)
	}
	// The corrupt partial is removed — never left to be mistaken for a
	// complete model.
	if _, statErr := os.Stat(filepath.Join(store.Root(), m.ID, m.File+".partial")); !os.IsNotExist(statErr) {
		t.Fatal("corrupt .partial must be removed")
	}
	if store.Installed(m) {
		t.Fatal("corrupt download must not land in place")
	}
}

func TestDownloaderResumesFromPartial(t *testing.T) {
	body := "0123456789abcdef"
	m := Model{
		ID: "resume-model", File: "r.bin", Bytes: int64(len(body)),
		SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body))),
	}
	var servedFrom int64 = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg := r.Header.Get("Range")
		if strings.HasPrefix(rg, "bytes=") {
			fmt.Sscanf(rg, "bytes=%d-", &servedFrom)
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, body[servedFrom:])
			return
		}
		servedFrom = 0
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	store := newTestStore(t)
	// Pre-plant a partial with the first half of the body.
	dir := filepath.Join(store.Root(), m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.File+".partial"), []byte(body[:8]), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := Downloader{Source: func(Model) string { return srv.URL }}
	if err := dl.downloadOne(store, m); err != nil {
		t.Fatalf("resume download: %v", err)
	}
	if servedFrom != 8 {
		t.Fatalf("expected resume from byte 8, served from %d", servedFrom)
	}
	if err := store.Verify(m); err != nil {
		t.Fatalf("verify after resume: %v", err)
	}
}

func TestDownloaderRestartsWhenServerIgnoresRange(t *testing.T) {
	body := "complete-body-bytes"
	m := Model{
		ID: "restart-model", File: "r.bin", Bytes: int64(len(body)),
		SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(body))),
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// Always 200 with the FULL body, even for a Range request.
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	store := newTestStore(t)
	dir := filepath.Join(store.Root(), m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.File+".partial"), []byte("stale-part"), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := Downloader{Source: func(Model) string { return srv.URL }}
	if err := dl.downloadOne(store, m); err != nil {
		t.Fatalf("restart download: %v", err)
	}
	if err := store.Verify(m); err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
}

func TestVerifyDetectsTruncation(t *testing.T) {
	m, body := fixtureModel(t, "full catalog bytes")
	store := newTestStore(t)
	dir := filepath.Join(store.Root(), m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, m.File), body[:5], 0o644); err != nil {
		t.Fatal(err)
	}
	if store.Installed(m) {
		t.Fatal("truncated file must not count as installed")
	}
	if err := store.Verify(m); err == nil {
		t.Fatal("verify must reject a truncated file")
	}
}
