// Package storage implements the server-side attachment file store:
// original + preview image variants keyed by (userID, attachmentID,
// variant) on the local filesystem. The layout is deliberately private
// (two hash-prefixed subdirectories — not a mirror of any client path
// scheme) so user-supplied values can never traverse outside the root.
//
// Writes land in a sibling .tmp file first and are renamed into place
// atomically; uploads are idempotent by content hash (the stored file
// carries the hash in its name, so a re-upload of the same bytes is a
// no-op). The interface leaves room for an S3-compatible backend later;
// this round ships only LocalFilesystemAttachmentStorage semantics.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// Variant names the stored rendition of an attachment.
type Variant string

const (
	// VariantOriginal is the full-resolution image.
	VariantOriginal Variant = "original"
	// VariantPreview is the list/thumbnail rendition.
	VariantPreview Variant = "preview"
)

func validVariant(v Variant) bool {
	return v == VariantOriginal || v == VariantPreview
}

// ErrInvalidName and friends are operator-facing sentinel errors.
var (
	ErrInvalidName  = errors.New("invalid attachment file name")
	ErrNotFound     = errors.New("attachment file not found")
	ErrHashMismatch = errors.New("attachment content hash mismatch")
)

// Store is the local-filesystem attachment backend. The zero value is
// not usable — use NewStore.
type Store struct {
	root string
}

// NewStore resolves root (creating it when empty-nested) and returns the
// store. root must be non-empty; directories are created on first write
// instead (MkdirAll per path) so a missing dir never blocks boot.
func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("attachment storage root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create attachment root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the configured storage root (diagnostics).
func (s *Store) Root() string { return s.root }

// fileName is <hash>_<variant>.bin — the hash makes re-uploads of the
// same bytes land on the identical path (idempotent writes for free).
func fileName(contentHash string, v Variant) string {
	return fmt.Sprintf("%s_%s.bin", contentHash, v)
}

// path builds <root>/<user[0:2]>/<user[2:4]>/<userID>/<attachmentID>/<file>.
// Every path component is derived from validated UUIDs or the hash
// charset, so traversal cannot be constructed through it.
func (s *Store) path(userID, attachmentID uuid.UUID, contentHash string, v Variant) (string, error) {
	if !validVariant(v) {
		return "", ErrInvalidName
	}
	for _, ch := range contentHash {
		valid := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
		if !valid {
			return "", ErrInvalidName
		}
	}
	u := userID.String()
	a := attachmentID.String()
	return filepath.Join(s.root, u[0:2], u[2:4], u, a, fileName(contentHash, v)), nil
}

// Write stores the variant of one attachment. Existing content for the
// same hash is verified byte-identical (cheap size + streaming hash
// check) and left alone — a retry never corrupts a good file. dst is
// written via a temporary file and an atomic rename.
func (s *Store) Write(ctx context.Context, userID, attachmentID uuid.UUID, contentHash string, v Variant, src io.Reader) (int64, error) {
	dst, err := s.path(userID, attachmentID, contentHash, v)
	if err != nil {
		return 0, err
	}
	if info, err := os.Stat(dst); err == nil && info.Mode().IsRegular() {
		// Idempotent re-upload: verify the stored copy matches the hash.
		ok, _, err := verifyFile(dst, contentHash)
		if err == nil && ok {
			return info.Size(), nil
		}
		// Corrupt/foreign file at the target: replace it below.
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("create attachment dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	h := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		tmp.Close()
		return 0, fmt.Errorf("write attachment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("close temp: %w", err)
	}
	// The streamed bytes must match the declared hash (the client's
	// contract); the server never persists mislabeled content.
	sum := hex.EncodeToString(h.Sum(nil))
	if contentHash != "" && sum != strings.ToLower(contentHash) {
		return 0, ErrHashMismatch
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return 0, fmt.Errorf("commit attachment: %w", err)
	}
	return size, nil
}

// Open returns a ReadSeekCloser for the stored variant. size reports the
// byte length (used for Content-Length and range-less responses).
func (s *Store) Open(userID, attachmentID uuid.UUID, v Variant) (*os.File, int64, error) {
	// The file name carries the hash, but callers only know the variant —
	// scan the attachment directory for the "<hash>_<variant>.bin" member.
	u := userID.String()
	a := attachmentID.String()
	dir := filepath.Join(s.root, u[0:2], u[2:4], u, a)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, ErrNotFound
	}
	suffix := "_" + string(v) + ".bin"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, suffix) {
			continue
		}
		if !validHexPrefix(name[:len(name)-len(suffix)]) {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return nil, 0, ErrNotFound
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, 0, ErrNotFound
		}
		return f, info.Size(), nil
	}
	return nil, 0, ErrNotFound
}

// Has reports whether the variant is stored.
func (s *Store) Has(userID, attachmentID uuid.UUID, v Variant) bool {
	f, _, err := s.Open(userID, attachmentID, v)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

// DeleteFiles removes every variant of one attachment (best-effort;
// the caller logs errors).
func (s *Store) DeleteFiles(userID, attachmentID uuid.UUID) error {
	u := userID.String()
	a := attachmentID.String()
	dir := filepath.Join(s.root, u[0:2], u[2:4], u, a)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete attachment files: %w", err)
	}
	// Prune now-empty parent dirs up to (but excluding) the root.
	for d := filepath.Dir(dir); d != s.root && strings.HasPrefix(d, s.root); d = filepath.Dir(d) {
		if err := os.Remove(d); err != nil {
			break // not empty (or already gone) — fine
		}
	}
	return nil
}

// DeleteUserFiles removes the whole per-user subtree (account deletion /
// cloud-data purge). Best-effort; callers log.
func (s *Store) DeleteUserFiles(userID uuid.UUID) error {
	u := userID.String()
	dir := filepath.Join(s.root, u[0:2], u[2:4], u)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete user attachments: %w", err)
	}
	for d := filepath.Dir(dir); d != s.root && strings.HasPrefix(d, s.root); d = filepath.Dir(d) {
		if err := os.Remove(d); err != nil {
			break
		}
	}
	return nil
}

func validHexPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		valid := (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
		if !valid {
			return false
		}
	}
	return true
}

func verifyFile(path, contentHash string) (bool, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return false, 0, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(contentHash), size, nil
}
