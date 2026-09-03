package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/config"
	syncpkg "livetranslate/server/internal/sync"
)

// §26: tombstone GC — rows deleted before the retention cutoff are
// hard-deleted; fresh tombstones survive so offline devices still learn
// about the deletion through the change log.
func TestTombstoneCleanup(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.TombstoneRetentionDays = 1 })
	u := e.registerAndVerify("gc@example.com", "correct-horse-battery-9")

	oldSess, newSess := uuid.New().String(), uuid.New().String()
	_, _, out := e.push(t, u.AccessToken,
		sessionOp(uuid.New().String(), oldSess, 0, "旧会话"),
		sessionOp(uuid.New().String(), newSess, 0, "新会话"))
	vOld := int(resultField(t, results(t, out)[0], "serverVersion").(float64))
	vNew := int(resultField(t, results(t, out)[1], "serverVersion").(float64))
	_, _, out = e.push(t, u.AccessToken,
		deleteOp(uuid.New().String(), "session", oldSess, vOld),
		deleteOp(uuid.New().String(), "session", newSess, vNew))

	// Age the OLD tombstone (and its change-log rows) beyond retention.
	past := time.Now().Add(-48 * time.Hour)
	for _, stmt := range []string{
		`UPDATE classroom_sessions SET deleted_at = $1 WHERE id = $2`,
		`UPDATE sync_changes SET created_at = $1 WHERE entity_id = $2`,
		`UPDATE processed_operations SET created_at = $1 WHERE entity_id = $2`,
	} {
		if _, err := testDB.Pool.Exec(t.Context(), stmt, past, oldSess); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	svc := syncpkg.NewService(e.cfg, testDB)
	if err := svc.RunTombstoneCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The aged tombstone is GONE (hard delete)…
	var n int
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM classroom_sessions WHERE id = $1`, oldSess).Scan(&n)
	if n != 0 {
		t.Fatal("aged tombstone survived GC")
	}
	// …its change-log rows too…
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sync_changes WHERE entity_id = $1`, oldSess).Scan(&n)
	if n != 0 {
		t.Fatal("aged change-log rows survived GC")
	}
	// …while the FRESH tombstone remains for offline devices.
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM classroom_sessions WHERE id = $1 AND deleted_at IS NOT NULL`, newSess).Scan(&n)
	if n != 1 {
		t.Fatal("fresh tombstone was garbage-collected too early")
	}
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	sawFresh := false
	for _, c := range changes {
		m := c.(map[string]any)
		if m["entityId"] == newSess && m["operation"] == "delete" {
			sawFresh = true
		}
	}
	if !sawFresh {
		t.Fatal("fresh delete missing from change log after GC")
	}
}
