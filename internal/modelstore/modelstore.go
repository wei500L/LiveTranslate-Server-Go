// Package modelstore implements the server-side on-device AI model store:
// the operator pre-downloads the client's offline-translation models
// (Hy-MT2 / MiLMMT GGUFs + the Gemma image-understanding model) once via
// `livetranslate-server download-models`; clients may then fetch them from
// their own server instead of Hugging Face (network-restricted regions,
// LAN classrooms, mobile-data-free sync setups).
//
// Layout: <root>/<model-id>/<filename> — every path component comes from
// the fixed catalog below (never user input), so traversal cannot be
// constructed through the HTTP routes. Downloads land in a sibling
// .partial file and are renamed into place only after the SHA256 check
// passes, mirroring the client installer's contract; a server file is
// only ever complete.
package modelstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Model is one downloadable client model. The values MUST stay in lockstep
// with the iOS app's Resources/ModelManifest.json (aiModels section): the
// id/filename/bytes/sha256 are the exact contract the client verifies
// against, whatever the download source.
type Model struct {
	ID       string // manifest key, e.g. "hy-mt2-1.8b-q4km"
	File     string // on-disk filename (also the install-relative path)
	Bytes    int64
	SHA256   string
	Upstream string // pinned-revision Hugging Face URL (pre-download source)
	Title    string // human-facing title for operator logs
}

// Catalog is the full set of client models this server can host. The
// upstream URLs pin immutable commit SHAs — never a moving branch.
func Catalog() []Model {
	return []Model{
		{
			ID:       "hy-mt2-1.8b-q4km",
			File:     "Hy-MT2-1.8B-Q4_K_M.gguf",
			Bytes:    1133080448,
			SHA256:   "dc5f44fcf1fa496ee7ad725982c0c8c553a4de00259b53af84c4b89fb0c06699",
			Upstream: "https://huggingface.co/tencent/Hy-MT2-1.8B-GGUF/resolve/1cd5208700acedef4ef93019b6cfc148b8522d45/Hy-MT2-1.8B-Q4_K_M.gguf",
			Title:    "Hy-MT2-1.8B Q4_K_M (offline ru->zh translation, default)",
		},
		{
			ID:       "milmmt-46-1b-q4km",
			File:     "MiLMMT-46-1B-v1.0.Q4_K_M.gguf",
			Bytes:    806057408,
			SHA256:   "74d38ba75108d455326e9deeaf9ab01bb266dfa665eae9c4aa84e84485d4fdf9",
			Upstream: "https://huggingface.co/mradermacher/MiLMMT-46-1B-v1.0-GGUF/resolve/34df5efbe6592773ec168cc7b307728c08623472/MiLMMT-46-1B-v1.0.Q4_K_M.gguf",
			Title:    "MiLMMT-46-1B Q4_K_M (battery-saver translation)",
		},
		{
			ID:       "gemma-4-e2b-it",
			File:     "gemma-4-E2B-it.litertlm",
			Bytes:    2588147712,
			SHA256:   "181938105e0eefd105961417e8da75903eacda102c4fce9ce90f50b97139a63c",
			Upstream: "https://huggingface.co/litert-community/gemma-4-E2B-it-litert-lm/resolve/b3ca0d2f076785a8f4b2219ddbd2bdb99954eae1/gemma-4-E2B-it.litertlm",
			Title:    "Gemma 4 E2B it (image understanding)",
		},
	}
}

// Lookup finds a catalog model by id.
func Lookup(id string) (Model, bool) {
	for _, m := range Catalog() {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// Sentinel errors (operator/handler facing).
var (
	ErrUnknownModel = errors.New("unknown model id")
	ErrNotInstalled = errors.New("model not downloaded on this server (run: livetranslate-server download-models)")
)

// Store is the on-disk model directory. The zero value is not usable.
type Store struct{ root string }

// NewStore resolves and creates the model root directory.
func NewStore(root string) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("model storage root must not be empty")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create model root: %w", err)
	}
	return &Store{root: root}, nil
}

// Root returns the configured root (diagnostics).
func (s *Store) Root() string { return s.root }

// path resolves the on-disk path for a catalog model. The id/file pair is
// validated against the catalog first — the returned path is therefore
// always catalog-derived, never a client-supplied path.
func (s *Store) path(m Model) string {
	return filepath.Join(s.root, m.ID, m.File)
}

// Installed reports whether the model's file exists with the exact
// expected size (a full SHA256 verify is available via Verify).
func (s *Store) Installed(m Model) bool {
	info, err := os.Stat(s.path(m))
	return err == nil && info.Size() == m.Bytes
}

// Verify streams the file through SHA256 and compares against the
// catalog. A size check runs first (cheap reject of truncated files).
func (s *Store) Verify(m Model) error {
	p := s.path(m)
	info, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotInstalled, m.ID)
	}
	if info.Size() != m.Bytes {
		return fmt.Errorf("size mismatch for %s: have %d bytes, catalog expects %d", m.ID, info.Size(), m.Bytes)
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hash %s: %w", m.ID, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("sha256 mismatch for %s: have %s, catalog expects %s — delete the file and re-run download-models", m.ID, got[:16]+"…", m.SHA256[:16]+"…")
	}
	return nil
}

// FilePath exposes the catalog-validated path for serving (http.ServeFile
// handles Range/HEAD/If-Range on it).
func (s *Store) FilePath(id, file string) (string, error) {
	m, ok := Lookup(id)
	if !ok || m.File != file {
		return "", ErrUnknownModel
	}
	return s.path(m), nil
}

// Downloader pre-fetches catalog models into a Store from their pinned
// upstream URLs. Source is overridable for tests; nil uses m.Upstream.
type Downloader struct {
	Source func(m Model) string
	Client *http.Client
	Log    func(format string, args ...any)
	// Progress, when set, receives byte-level progress for the current
	// model: (bytesReceivedSoFar, totalBytes). Called from the download
	// goroutine — keep it cheap (a shared *progressWriter fans bytes in).
	Progress func(received, total int64)
	// OnModelStart/OnModelDone bracket each model's download (verbatim
	// after the "downloading…" log line / before the command exits or
	// moves on). The CLI uses them to reset its progress timer and label.
	OnModelStart func(m Model)
	OnModelDone  func(m Model)
}

func (d Downloader) source(m Model) string {
	if d.Source != nil {
		return d.Source(m)
	}
	return m.Upstream
}

func (d Downloader) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	// No timeout by default mirrors http.DefaultClient, but downloads are
	// multi-GB and a hung TCP connection (no FIN, no reset) would stall the
	// command forever with no output. ResponseHeaderTimeout rejects a
	// silent upstream before the first byte; a read-idle deadline (via
	// http.ResponseController, see downloadOne) kills a connection that
	// goes silent mid-body without punishing slow-but-alive transfers.
	// Callers that supply their own Client own this policy.
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
		},
	}
}

func (d Downloader) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log(format, args...)
	}
}

// DownloadAll ensures every catalog model is present and hash-verified.
// Already-verified models are skipped (idempotent); each file resumes
// from a leftover .partial sibling via HTTP Range when the server
// supports it, restarting the file otherwise.
func (d Downloader) DownloadAll(store *Store) error {
	for _, m := range Catalog() {
		if store.Installed(m) {
			if err := store.Verify(m); err == nil {
				d.logf("%s: already present and verified, skipping", m.ID)
				continue
			}
			d.logf("%s: present but FAILED verification — re-downloading", m.ID)
			_ = os.Remove(store.path(m))
		}
		d.logf("%s: downloading %s (%.2f GiB)", m.ID, m.Title, float64(m.Bytes)/(1<<30))
		if d.OnModelStart != nil {
			d.OnModelStart(m)
		}
		if err := d.downloadOne(store, m); err != nil {
			return fmt.Errorf("%s: %w", m.ID, err)
		}
		d.logf("%s: verified (sha256 ok)", m.ID)
		if d.OnModelDone != nil {
			d.OnModelDone(m)
		}
	}
	return nil
}

func (d Downloader) downloadOne(store *Store, m Model) error {
	dest := store.path(m)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	partial := dest + ".partial"

	// Resume from a leftover partial when it is smaller than the target.
	var resumeFrom int64
	if info, err := os.Stat(partial); err == nil && info.Size() < m.Bytes {
		resumeFrom = info.Size()
	} else if err == nil {
		_ = os.Remove(partial) // full-size but unverified: restart
	}

	req, err := http.NewRequest(http.MethodGet, d.source(m), nil)
	if err != nil {
		return err
	}
	if resumeFrom > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeFrom))
	}
	resp, err := d.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	offset := resumeFrom
	// A server that ignores Range answers 200 with the full body.
	if resp.StatusCode == http.StatusOK {
		offset = 0
		_ = os.Remove(partial)
	} else if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			f.Close()
			return err
		}
	}
	// Byte-level progress through the download body; the hash walk below
	// re-uses the sink to keep the CLI's percentage honest end to end.
	// received starts at the resume offset so percentages are absolute.
	// Read-idle deadline: a connection that goes silent for
	// readIdleTimeout aborts the copy with an error (the operator re-runs
	// and resumes from .partial) instead of stalling the command forever.
	body := &deadlineBody{Response: resp}
	pw := &progressWriter{
		onChunk:  d.Progress,
		total:    m.Bytes,
		received: offset,
		nextAt:   offset,
		poke:     body.poke,
	}
	_, copyErr := io.Copy(io.MultiWriter(f, pw), body)
	closeErr := f.Close()
	pw.finish()
	if copyErr != nil {
		return fmt.Errorf("download: %w", copyErr)
	}
	if closeErr != nil {
		return closeErr
	}

	// Size + hash gate before the file is allowed into place.
	info, err := os.Stat(partial)
	if err != nil {
		return err
	}
	if info.Size() != m.Bytes {
		return fmt.Errorf("downloaded %d bytes, expected %d", info.Size(), m.Bytes)
	}
	h := sha256.New()
	pf, err := os.Open(partial)
	if err != nil {
		return err
	}
	_, hashErr := io.Copy(h, pf)
	pf.Close()
	if hashErr != nil {
		return hashErr
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		_ = os.Remove(partial)
		return fmt.Errorf("sha256 mismatch: have %s, expected %s", got[:16]+"…", m.SHA256[:16]+"…")
	}
	return os.Rename(partial, dest)
}

// progressWriter fans download bytes into the Downloader.Progress hook,
// throttled to at most one callback per interval (an unthrottled per-Read
// callback on a fast link would flood the CLI logger). Every Write rolls
// the body's read deadline via poke so a silent connection is cut after
// readIdleTimeout.
type progressWriter struct {
	onChunk  func(received, total int64)
	poke     func()
	total    int64
	received int64
	nextAt   int64
	finished bool
}

const progressIntervalBytes = 32 << 20 // report every 32 MiB (and at end)
const readIdleTimeout = 5 * time.Minute

func (p *progressWriter) Write(b []byte) (int, error) {
	if p.poke != nil {
		p.poke()
	}
	p.received += int64(len(b))
	if p.onChunk != nil && p.received >= p.nextAt {
		p.nextAt = p.received + progressIntervalBytes
		p.onChunk(p.received, p.total)
	}
	return len(b), nil
}

// finish flushes the final callback exactly once.
func (p *progressWriter) finish() {
	if p.finished || p.onChunk == nil {
		return
	}
	p.finished = true
	p.onChunk(p.received, p.total)
}

// deadlineBody wraps a response body with a rolling read deadline: each
// successful Read (surfaced through poke) extends it; a connection that
// delivers nothing for readIdleTimeout fails the next Read with the
// deadline error. If the underlying transport does not support deadlines
// (SetReadDeadline errors), reads proceed without one — same behavior as
// http.DefaultClient, never worse.
type deadlineBody struct {
	Response *http.Response
	lastErr  error
}

func (b *deadlineBody) poke() {
	// Deadline errors are expected once the idle timeout trips; do not
	// re-arm after that.
	if b.lastErr == nil {
		b.lastErr = b.setDeadline(time.Now().Add(readIdleTimeout))
	}
}

func (b *deadlineBody) setDeadline(t time.Time) error {
	conn := b.Response.Body
	for {
		d, ok := conn.(interface{ SetReadDeadline(time.Time) error })
		if ok {
			return d.SetReadDeadline(t)
		}
		u, ok := conn.(interface{ Unwrap() error })
		if !ok {
			return nil // no deadline support: proceed without
		}
		if err := u.Unwrap(); err != nil {
			return err // wrapper holds an error, e.g. the tripped deadline
		}
	}
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	return b.Response.Body.Read(p)
}
