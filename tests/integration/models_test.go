package integration

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"livetranslate/server/internal/modelstore"
)

// modelsTestEnv builds the standard env plus a plant helper that writes a
// fixture file into the model store as if download-models had fetched it.
// The fixture reuses a REAL catalog entry with adjusted bytes/sha (the
// route contract is id/file from the catalog; content is opaque to the
// server — the client verifies hashes).
func modelsTestEnv(t *testing.T) (*env, func(m modelstore.Model, body string)) {
	t.Helper()
	e := newEnv(t, nil)
	plant := func(m modelstore.Model, body string) {
		t.Helper()
		dir := filepath.Join(e.modelStore.Root(), m.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, m.File), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return e, plant
}

func TestModelRoutesRequireAuth(t *testing.T) {
	e, _ := modelsTestEnv(t)

	resp, body := e.get("/v1/models/ai", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("index without token: %d %s", resp.StatusCode, body)
	}
	resp, body = e.get("/v1/models/ai/hy-mt2-1.8b-q4km/Hy-MT2-1.8B-Q4_K_M.gguf", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("download without token: %d %s", resp.StatusCode, body)
	}
}

func TestModelIndexListsCatalogWithPresence(t *testing.T) {
	e, plant := modelsTestEnv(t)

	// Plant the first catalog model (content doesn't matter for the
	// index; Installed only checks presence+size — plant with the real
	// size so presence is truthful).
	hy, _ := modelstore.Lookup("hy-mt2-1.8b-q4km")
	plant(hy, strings.Repeat("x", int(hy.Bytes)))

	login := e.registerAndVerify("models-index@example.com", "passpass123")
	resp, body := e.get("/v1/models/ai", login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("index: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"id":"hy-mt2-1.8b-q4km"`) ||
		!strings.Contains(body, `"id":"milmmt-46-1b-q4km"`) ||
		!strings.Contains(body, `"id":"gemma-4-e2b-it"`) {
		t.Fatalf("index must list the full catalog: %s", body)
	}
	// The planted model reports installed; the others honestly don't.
	if !strings.Contains(body, `"id":"hy-mt2-1.8b-q4km","file":"Hy-MT2-1.8B-Q4_K_M.gguf","bytes":1133080448,"sha256":"dc5f44fcf1fa496ee7ad725982c0c8c553a4de00259b53af84c4b89fb0c06699","installed":true`) {
		t.Fatalf("planted model must report installed=true with the catalog contract: %s", body)
	}
	if !strings.Contains(body, `"id":"gemma-4-e2b-it"`) || !strings.Contains(body, `"installed":false`) {
		t.Fatalf("absent model must report installed=false: %s", body)
	}
}

func TestModelDownloadServesRangeAndRejectsUnknown(t *testing.T) {
	e, plant := modelsTestEnv(t)

	hy, _ := modelstore.Lookup("hy-mt2-1.8b-q4km")
	body := strings.Repeat("m", int(hy.Bytes))
	plant(hy, body)
	login := e.registerAndVerify("models-dl@example.com", "passpass123")

	// Full download: exact bytes, ETag-less but correct length.
	resp, raw := e.get("/v1/models/ai/hy-mt2-1.8b-q4km/Hy-MT2-1.8B-Q4_K_M.gguf", login.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d %s", resp.StatusCode, raw)
	}
	if resp.ContentLength != hy.Bytes {
		t.Fatalf("content length %d, want %d", resp.ContentLength, hy.Bytes)
	}

	// Range request — the client installer's pause/resume depends on it.
	req, err := http.NewRequest(http.MethodGet, e.api.URL+"/v1/models/ai/hy-mt2-1.8b-q4km/Hy-MT2-1.8B-Q4_K_M.gguf", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+login.AccessToken)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", hy.Bytes-16))
	rresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range request: %d (want 206)", rresp.StatusCode)
	}
	tail, err := io.ReadAll(rresp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 16 || string(tail) != strings.Repeat("m", 16) {
		t.Fatalf("range tail wrong: %q", tail)
	}

	// Unknown model / traversal: catalog-validated 404, never a path.
	resp, raw = e.get("/v1/models/ai/../../etc/passwd", login.AccessToken)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("traversal must not resolve: %d %s", resp.StatusCode, raw)
	}
	resp, raw = e.get("/v1/models/ai/no-such/x.bin", login.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown model: %d %s", resp.StatusCode, raw)
	}

	// A known model that the operator has NOT pre-downloaded: honest 404
	// with the operator action in the detail.
	resp, raw = e.get("/v1/models/ai/gemma-4-e2b-it/gemma-4-E2B-it.litertlm", login.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("absent model: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "download-models") {
		t.Fatalf("absent-model detail must name the operator action: %s", raw)
	}
}
