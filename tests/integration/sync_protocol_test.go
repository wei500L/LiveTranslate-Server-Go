package integration

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"livetranslate/server/internal/token"
)

// push posts one operation batch as the iOS client would.
func (e *env) push(t *testing.T, token string, ops ...map[string]any) (*http.Response, string, map[string]any) {
	t.Helper()
	body := map[string]any{"schemaVersion": 1, "operations": ops}
	resp, raw := e.do(http.MethodPost, "/v1/sync/push", token, body)
	var out map[string]any
	if resp.StatusCode == http.StatusOK {
		decode(t, raw, &out)
	}
	return resp, raw, out
}

func results(t *testing.T, body map[string]any) []any {
	t.Helper()
	rs, ok := body["results"].([]any)
	if !ok {
		t.Fatalf("no results array: %v", body)
	}
	return rs
}

func resultField(t *testing.T, r any, field string) any {
	t.Helper()
	m, ok := r.(map[string]any)
	if !ok {
		t.Fatalf("result not an object: %v", r)
	}
	return m[field]
}

// sessionOp builds a session upsert carrying the minimum iOS payload.
func sessionOp(opID, entityID string, baseVersion int, title string) map[string]any {
	return map[string]any{
		"operationId":     opID,
		"entityType":      "session",
		"entityId":        entityID,
		"operation":       "upsert",
		"baseVersion":     baseVersion,
		"clientUpdatedAt": "2026-09-01T10:00:00Z",
		"payload": map[string]any{
			"sessionId":     entityID,
			"title":         title,
			"sessionStatus": "completed",
			"startedAt":     "2026-09-01T09:00:00Z",
			"endedAt":       "2026-09-01T09:50:00Z",
			"duration":      3000.0,
		},
	}
}

func entryOp(opID, sessionID, entityID string, seq int, russian string) map[string]any {
	return map[string]any{
		"operationId":     opID,
		"entityType":      "entry",
		"entityId":        entityID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-01T10:00:01Z",
		"payload": map[string]any{
			"sessionId":         sessionID,
			"entryId":           entityID,
			"sequenceId":        seq,
			"startOffset":       float64(seq * 10),
			"endOffset":         float64(seq*10 + 8),
			"russianText":       russian,
			"chineseText":       "第" + fmt.Sprint(seq) + "句",
			"translationStatus": "completed",
		},
	}
}

func deleteOp(opID, entityType, entityID string, baseVersion int) map[string]any {
	return map[string]any{
		"operationId":     opID,
		"entityType":      entityType,
		"entityId":        entityID,
		"operation":       "delete",
		"baseVersion":     baseVersion,
		"clientUpdatedAt": "2026-09-01T11:00:00Z",
		"payload":         map[string]any{},
	}
}

// pull fetches changes after cursor.
func (e *env) pull(t *testing.T, token string, cursor int64, limit int) (changes []any, next int64, hasMore bool) {
	t.Helper()
	path := fmt.Sprintf("/v1/sync/pull?cursor=%d", cursor)
	if limit > 0 {
		path += fmt.Sprintf("&limit=%d", limit)
	}
	resp, raw := e.get(path, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull: %d %s", resp.StatusCode, raw)
	}
	var out struct {
		Changes    []any `json:"changes"`
		NextCursor int64 `json:"nextCursor"`
		HasMore    bool  `json:"hasMore"`
	}
	decode(t, raw, &out)
	return out.Changes, out.NextCursor, out.HasMore
}

// §26 (iOS↔Go push/pull): a full round trip of every entity type over the
// real HTTP surface, in the exact wire shape iOS uses.
func TestSyncPushPullRoundTrip(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("sync1@example.com", "correct-horse-battery-9")

	sessID, entryID, bmID, favID := uuid.New().String(), uuid.New().String(), uuid.New().String(), uuid.New().String()
	resp, raw, body := e.push(t, u.AccessToken,
		sessionOp(uuid.New().String(), sessID, 0, "第一课：俄语语法"),
		entryOp(uuid.New().String(), sessID, entryID, 1, "Привет, как дела?"),
		map[string]any{ // bookmark
			"operationId":     uuid.New().String(),
			"entityType":      "bookmark",
			"entityId":        bmID,
			"operation":       "upsert",
			"baseVersion":     0,
			"clientUpdatedAt": "2026-09-01T10:00:02Z",
			"payload": map[string]any{
				"sessionId": sessID, "entryId": entryID, "isBookmarked": true,
			},
		},
		map[string]any{ // favorite
			"operationId":     uuid.New().String(),
			"entityType":      "favorite",
			"entityId":        favID,
			"operation":       "upsert",
			"baseVersion":     0,
			"clientUpdatedAt": "2026-09-01T10:00:03Z",
			"payload":         map[string]any{"sessionId": sessID, "isFavorite": true},
		},
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: %d %s", resp.StatusCode, raw)
	}
	rs := results(t, body)
	if len(rs) != 4 {
		t.Fatalf("results = %d, want 4", len(rs))
	}
	for i, r := range rs {
		if resultField(t, r, "status") != "accepted" {
			t.Fatalf("result %d not accepted: %v", i, r)
		}
		if v := resultField(t, r, "serverVersion"); v == nil || int(v.(float64)) < 1 {
			t.Fatalf("result %d missing serverVersion: %v", i, r)
		}
	}
	// The response envelope carries schemaVersion + serverTime.
	if body["schemaVersion"] != float64(1) || body["serverTime"] == nil {
		t.Fatalf("push envelope wrong: %v", body)
	}

	changes, next, hasMore := e.pull(t, u.AccessToken, 0, 0)
	if hasMore || next == 0 {
		t.Fatalf("pull flags: hasMore=%v next=%d", hasMore, next)
	}
	if len(changes) != 4 {
		t.Fatalf("pulled %d changes, want 4", len(changes))
	}
	// Every record round-trips its entity type and payload fields.
	seen := map[string]bool{}
	for _, c := range changes {
		m := c.(map[string]any)
		seen[m["entityType"].(string)] = true
		rec := m["record"].(map[string]any)
		if rec["deleted"] != false {
			t.Fatalf("live record flagged deleted: %v", rec)
		}
		if et := m["entityType"]; et == "session" {
			if rec["title"] != "第一课：俄语语法" {
				t.Fatalf("session title lost: %v", rec)
			}
		}
	}
	for _, et := range []string{"session", "entry", "bookmark", "favorite"} {
		if !seen[et] {
			t.Fatalf("entity type %s missing from pull", et)
		}
	}

	// Pull again from nextCursor: nothing new.
	changes, _, _ = e.pull(t, u.AccessToken, next, 0)
	if len(changes) != 0 {
		t.Fatalf("re-pull returned %d changes, want 0", len(changes))
	}

	// Status counts match what was written.
	resp, raw = e.get("/v1/sync/status", u.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", resp.StatusCode, raw)
	}
	var st struct {
		SessionCount  int   `json:"sessionCount"`
		EntryCount    int   `json:"entryCount"`
		ChangeLogTail int64 `json:"changeLogTail"`
	}
	decode(t, raw, &st)
	if st.SessionCount != 1 || st.EntryCount != 1 || st.ChangeLogTail < 4 {
		t.Fatalf("status counts: %+v", st)
	}
}

// §26: operationId idempotency — a retried push replays the stored result
// and writes nothing new.
func TestSyncIdempotency(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("sync2@example.com", "correct-horse-battery-9")

	opID, sessID := uuid.New().String(), uuid.New().String()
	_, _, first := e.push(t, u.AccessToken, sessionOp(opID, sessID, 0, "幂等测试"))
	r1 := results(t, first)[0]
	v1 := int(resultField(t, r1, "serverVersion").(float64))

	// Same operationId, DIFFERENT payload (a retry race): the stored result
	// replays verbatim; the new payload is ignored.
	op := sessionOp(opID, sessID, v1, "幂等测试-改")
	op["payload"].(map[string]any)["title"] = "SHOULD-NOT-BE-WRITTEN"
	_, _, second := e.push(t, u.AccessToken, op)
	r2 := results(t, second)[0]
	if resultField(t, r2, "status") != "accepted" {
		t.Fatalf("replay status = %v", resultField(t, r2, "status"))
	}
	if int(resultField(t, r2, "serverVersion").(float64)) != v1 {
		t.Fatalf("replay serverVersion = %v, want %d", resultField(t, r2, "serverVersion"), v1)
	}

	// No duplicate rows, no new change-log entries.
	var rows, logs int
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM classroom_sessions`).Scan(&rows)
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM sync_changes`).Scan(&logs)
	if rows != 1 || logs != 1 {
		t.Fatalf("idempotent replay duplicated state: sessions=%d changes=%d", rows, logs)
	}
	// The title in the DB is the ORIGINAL one.
	var title string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT title FROM classroom_sessions`).Scan(&title)
	if title != "幂等测试" {
		t.Fatalf("replay overwrote title: %q", title)
	}
}

// §26: stale baseVersion → conflict with serverRecord; future baseVersion
// → rejected "schema" (parity with the Python service).
func TestSyncConflicts(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("sync3@example.com", "correct-horse-battery-9")

	sessID := uuid.New().String()
	_, _, first := e.push(t, u.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "版本一"))
	v1 := int(resultField(t, results(t, first)[0], "serverVersion").(float64))

	// Stale base (0 < v1) with a different payload → conflict + serverRecord.
	stale := sessionOp(uuid.New().String(), sessID, 0, "过期的修改")
	_, _, out := e.push(t, u.AccessToken, stale)
	r := results(t, out)[0]
	if resultField(t, r, "status") != "conflict" {
		t.Fatalf("stale push status = %v", r)
	}
	if resultField(t, r, "errorCode") != "stale_base_version" {
		t.Fatalf("conflict errorCode = %v", resultField(t, r, "errorCode"))
	}
	rec := resultField(t, r, "serverRecord").(map[string]any)
	if rec["title"] != "版本一" || int(rec["serverVersion"].(float64)) != v1 {
		t.Fatalf("serverRecord wrong: %v", rec)
	}
	// Matching baseVersion merges fine (no conflict).
	merge := sessionOp(uuid.New().String(), sessID, v1, "版本二")
	_, _, out = e.push(t, u.AccessToken, merge)
	r = results(t, out)[0]
	if resultField(t, r, "status") != "accepted" {
		t.Fatalf("in-sync push status = %v", r)
	}
	v2 := int(resultField(t, r, "serverVersion").(float64))
	if v2 != v1+1 {
		t.Fatalf("serverVersion %d after update, want %d", v2, v1+1)
	}

	// Future base (v2+99) → rejected with errorCode "schema".
	future := sessionOp(uuid.New().String(), sessID, v2+99, "来自未来")
	_, _, out = e.push(t, u.AccessToken, future)
	r = results(t, out)[0]
	if resultField(t, r, "status") != "rejected" || resultField(t, r, "errorCode") != "schema" {
		t.Fatalf("future-base push = %v", r)
	}

	// Unknown entity type → rejected.
	weird := sessionOp(uuid.New().String(), uuid.New().String(), 0, "x")
	weird["entityType"] = "galaxy"
	_, _, out = e.push(t, u.AccessToken, weird)
	r = results(t, out)[0]
	if resultField(t, r, "status") != "rejected" {
		t.Fatalf("unknown entity push = %v", r)
	}
}

// §26: delete-wins — a tombstoned entity cannot be resurrected by an upsert.
func TestSyncDeleteWins(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("sync4@example.com", "correct-horse-battery-9")

	sessID := uuid.New().String()
	_, _, first := e.push(t, u.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "将被删除"))
	v1 := int(resultField(t, results(t, first)[0], "serverVersion").(float64))

	_, _, out := e.push(t, u.AccessToken, deleteOp(uuid.New().String(), "session", sessID, v1))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("delete not accepted: %v", out)
	}

	// Resurrection attempt (base matches the tombstone version) → conflict,
	// serverRecord shows deleted=true.
	res := sessionOp(uuid.New().String(), sessID, v1+1, "复活尝试")
	_, _, out = e.push(t, u.AccessToken, res)
	r := results(t, out)[0]
	if resultField(t, r, "status") != "conflict" {
		t.Fatalf("resurrection status = %v", r)
	}
	rec := resultField(t, r, "serverRecord").(map[string]any)
	if rec["deleted"] != true {
		t.Fatalf("tombstone record not marked deleted: %v", rec)
	}

	// Pull shows the delete change; per the wire contract (Python parity)
	// delete changes carry record: null — iOS applies the tombstone by id.
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	var sawDelete bool
	for _, c := range changes {
		m := c.(map[string]any)
		if m["operation"] == "delete" && m["entityType"] == "session" {
			sawDelete = true
			if m["record"] != nil {
				t.Fatalf("delete change carries a record (wire drift): %v", m)
			}
		}
	}
	if !sawDelete {
		t.Fatalf("no delete change in pull: %v", changes)
	}
	// The row is tombstoned, not erased.
	var deletedAt *time.Time
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT deleted_at FROM classroom_sessions WHERE id = $1`, sessID).Scan(&deletedAt)
	if deletedAt == nil {
		t.Fatal("session row not tombstoned")
	}
}

// §26: cascade delete — deleting a session tombstones its entries,
// bookmarks and favorites too.
func TestSyncCascadeDelete(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("sync5@example.com", "correct-horse-battery-9")

	sessID := uuid.New().String()
	entryID := uuid.New().String()
	bmID := uuid.New().String()
	favID := uuid.New().String()
	_, _, first := e.push(t, u.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "级联测试"))
	v1 := int(resultField(t, results(t, first)[0], "serverVersion").(float64))
	_, _, _ = e.push(t, u.AccessToken,
		entryOp(uuid.New().String(), sessID, entryID, 1, "Каскадная запись"),
		map[string]any{
			"operationId": uuid.New().String(), "entityType": "bookmark", "entityId": bmID,
			"operation": "upsert", "baseVersion": 0, "clientUpdatedAt": "2026-09-01T10:00:02Z",
			"payload": map[string]any{"sessionId": sessID, "entryId": entryID, "isBookmarked": true},
		},
		map[string]any{
			"operationId": uuid.New().String(), "entityType": "favorite", "entityId": favID,
			"operation": "upsert", "baseVersion": 0, "clientUpdatedAt": "2026-09-01T10:00:03Z",
			"payload": map[string]any{"sessionId": sessID, "isFavorite": true},
		},
	)

	_, _, out := e.push(t, u.AccessToken, deleteOp(uuid.New().String(), "session", sessID, v1))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("session delete failed: %v", out)
	}

	// Every child row is tombstoned in the DB…
	for table, id := range map[string]string{
		"transcript_entries": entryID, "bookmarks": bmID, "favorite_sessions": favID,
	} {
		var deletedAt *time.Time
		err := testDB.Pool.QueryRow(t.Context(),
			fmt.Sprintf(`SELECT deleted_at FROM %s WHERE id = $1`, table), id).Scan(&deletedAt)
		if err != nil || deletedAt == nil {
			t.Fatalf("%s not cascade-tombstoned (err=%v deleted=%v)", table, err, deletedAt)
		}
	}
	// …and the change log carries delete changes for all four entities.
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	delTypes := map[string]int{}
	for _, c := range changes {
		m := c.(map[string]any)
		if m["operation"] == "delete" {
			delTypes[m["entityType"].(string)]++
		}
	}
	if delTypes["session"] != 1 || delTypes["entry"] != 1 || delTypes["bookmark"] != 1 || delTypes["favorite"] != 1 {
		t.Fatalf("cascade delete changes wrong: %v", delTypes)
	}
}

// §26: user isolation — one user's data is invisible to another.
func TestSyncUserIsolation(t *testing.T) {
	e := newEnv(t, nil)
	alice := e.registerAndVerify("alice@example.com", "correct-horse-battery-9")
	bob := e.registerAndVerify("bob@example.com", "correct-horse-battery-9")

	sessID := uuid.New().String()
	e.push(t, alice.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "Alice 的课堂"))

	// Bob pulls from zero: nothing.
	changes, _, _ := e.pull(t, bob.AccessToken, 0, 0)
	if len(changes) != 0 {
		t.Fatalf("Bob saw %d of Alice's changes", len(changes))
	}
	// Bob cannot address Alice's row by entity id. The id is a global
	// primary key (Python schema parity), so the duplicate insert fails —
	// Python surfaces this as IntegrityError→500, and so does Go. The
	// security invariant: no merge, no leak, Alice's row untouched.
	resp, raw, _ := e.push(t, bob.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "Bob 冒用 id"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("Bob's same-id push = %d (%s), want 500 (Python parity)", resp.StatusCode, raw)
	}
	var rows int
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT count(*) FROM classroom_sessions WHERE id = $1`, sessID).Scan(&rows)
	if rows != 1 {
		t.Fatalf("same session id across users = %d rows, want 1 (no cross-user row)", rows)
	}
	var title string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT title FROM classroom_sessions WHERE id = $1`, sessID).Scan(&title)
	if title != "Alice 的课堂" {
		t.Fatalf("Alice's row altered by Bob: %q", title)
	}
	// Bob's pull still shows nothing of Alice's.
	bchanges, _, _ := e.pull(t, bob.AccessToken, 0, 0)
	if len(bchanges) != 0 {
		t.Fatalf("Bob's pull saw Alice's data: %v", bchanges)
	}
}

// §26: change_sequence cursor pagination — limit slices the stream,
// nextCursor resumes exactly, sequences are strictly increasing.
func TestSyncCursorPagination(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("page@example.com", "correct-horse-battery-9")

	ops := make([]map[string]any, 0, 7)
	for i := 0; i < 7; i++ {
		ops = append(ops, sessionOp(uuid.New().String(), uuid.New().String(), 0,
			fmt.Sprintf("分页-%02d", i)))
	}
	_, _, out := e.push(t, u.AccessToken, ops...)
	if len(results(t, out)) != 7 {
		t.Fatalf("batch push results = %d", len(results(t, out)))
	}

	var got []any
	cursor := int64(0)
	var lastSeq int64 = -1
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("pagination did not terminate")
		}
		changes, next, hasMore := e.pull(t, u.AccessToken, cursor, 3)
		for _, c := range changes {
			seq := int64(c.(map[string]any)["changeSequence"].(float64))
			if seq <= lastSeq {
				t.Fatalf("change_sequence not increasing: %d after %d", seq, lastSeq)
			}
			lastSeq = seq
		}
		got = append(got, changes...)
		cursor = next
		if !hasMore {
			break
		}
	}
	if len(got) != 7 {
		t.Fatalf("paged pull collected %d changes, want 7", len(got))
	}
	// A second user's activity advances the GLOBAL sequence monotonically —
	// cursors are global (bigserial), per-user filtered.
	other := e.registerAndVerify("page2@example.com", "correct-horse-battery-9")
	e.push(t, other.AccessToken, sessionOp(uuid.New().String(), uuid.New().String(), 0, "另一用户"))
	changes, _, _ := e.pull(t, u.AccessToken, cursor, 0)
	if len(changes) != 0 {
		t.Fatalf("cross-user leak through cursor: %v", changes)
	}
	_, _, _ = e.pull(t, other.AccessToken, 0, 0) // sanity: other sees own
}

// §26: schema-version gate — an unsupported client schema is rejected with
// the machine-readable errorCode iOS uses to show 需要更新 App.
func TestSyncSchemaGate(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("schema@example.com", "correct-horse-battery-9")

	resp, raw := e.do(http.MethodPost, "/v1/sync/push", u.AccessToken, map[string]any{
		"schemaVersion": 99,
		"operations":    []any{sessionOp(uuid.New().String(), uuid.New().String(), 0, "x")},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("schema gate status = %d: %s", resp.StatusCode, raw)
	}
	var er map[string]any
	decode(t, raw, &er)
	detail := er["detail"].(map[string]any)
	if detail["errorCode"] != "client_schema_unsupported" {
		t.Fatalf("errorCode = %v", detail["errorCode"])
	}
	if detail["maxClientSchemaVersion"] != float64(1) {
		t.Fatalf("maxClientSchemaVersion = %v", detail["maxClientSchemaVersion"])
	}
}

// §26: unverified accounts can never reach sync (defense in depth beyond
// the login refusal).
func TestSyncRejectsBadTokens(t *testing.T) {
	e := newEnv(t, nil)
	e.registerAndVerify("tok@example.com", "correct-horse-battery-9")

	for name, hdr := range map[string]string{
		"missing":   "",
		"garbage":   "Bearer not-a-jwt",
		"wrong-key": "Bearer " + forgedToken(e),
	} {
		req, _ := http.NewRequest(http.MethodGet, e.api.URL+"/v1/sync/status", nil)
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if name == "missing" && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		if name != "missing" && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: %d, want 401", name, resp.StatusCode)
		}
	}
}

// forgedToken mints a JWT signed with a DIFFERENT key (same claims shape).
func forgedToken(e *env) string {
	// Forge with a DIFFERENT signing key (same claims shape) — the server
	// must reject the signature.
	cfg := *e.cfg
	cfg.JWTSecret = "attacker-knows-this-key"
	m := token.NewManager(&cfg)
	tok, _, err := m.NewAccessToken("00000000-0000-0000-0000-000000000000",
		"00000000-0000-0000-0000-000000000000", "user")
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

// §Correction sync: transcript corrections ride as their own entity
// (id == entry id) with newer-modifiedAt-wins merge and delete-wins
// revert semantics. The entry's model text stays immutable.
func TestSyncTranscriptCorrectionLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("correction@example.com", "correct-horse-battery-9")

	sessID, entryID := uuid.New().String(), uuid.New().String()
	_, _, out := e.push(t, u.AccessToken,
		sessionOp(uuid.New().String(), sessID, 0, "有修正的课堂"),
		entryOp(uuid.New().String(), sessID, entryID, 1, "Привет"),
	)
	if resultField(t, results(t, out)[1], "status") != "accepted" {
		t.Fatalf("entry push failed: %v", out)
	}

	// Correction upsert (id == entry id).
	corrOp := func(base int, russian string, chinese any, modified string) map[string]any {
		return map[string]any{
			"operationId":     uuid.New().String(),
			"entityType":      "transcript_correction",
			"entityId":        entryID,
			"operation":       "upsert",
			"baseVersion":     base,
			"clientUpdatedAt": modified,
			"payload": map[string]any{
				"correctionRussian":    russian,
				"correctionChinese":    chinese,
				"correctionModifiedAt": modified,
			},
		}
	}
	_, _, out = e.push(t, u.AccessToken,
		corrOp(0, "Привет, мир!", nil, "2026-09-01T11:00:00Z"))
	r := results(t, out)[0]
	if resultField(t, r, "status") != "accepted" {
		t.Fatalf("correction push failed: %v", r)
	}
	v1 := int(resultField(t, r, "serverVersion").(float64))
	if v1 != 1 {
		t.Fatalf("correction serverVersion = %d, want 1", v1)
	}

	// Older modifiedAt on the same base: the NEWER text wins, version bumps.
	_, _, out = e.push(t, u.AccessToken,
		corrOp(1, "старая правка", "旧修改", "2026-09-01T10:00:00Z"))
	r = results(t, out)[0]
	if resultField(t, r, "status") != "accepted" {
		t.Fatalf("older correction push failed: %v", r)
	}
	var russian string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT russian_text FROM transcript_corrections WHERE id = $1`, entryID).Scan(&russian)
	if russian != "Привет, мир!" {
		t.Fatalf("older correction overwrote newer text: %q", russian)
	}

	// The entry's model text is untouched by correction writes.
	var modelRussian string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT russian_text FROM transcript_entries WHERE id = $1`, entryID).Scan(&modelRussian)
	if modelRussian != "Привет" {
		t.Fatalf("model text changed: %q", modelRussian)
	}

	// Correction without a live parent entry → rejected.
	orphan := corrOp(0, "x", nil, "2026-09-01T12:00:00Z")
	orphan["entityId"] = uuid.New().String()
	_, _, out = e.push(t, u.AccessToken, orphan)
	if resultField(t, results(t, out)[0], "status") != "rejected" {
		t.Fatalf("orphan correction accepted: %v", out)
	}

	// Delete (revert to model original) then resurrection attempt → delete-wins.
	_, _, out = e.push(t, u.AccessToken, deleteOp(uuid.New().String(), "transcript_correction", entryID, 0))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("correction delete failed: %v", out)
	}
	_, _, out = e.push(t, u.AccessToken,
		corrOp(99, "воскрес", nil, "2026-09-01T13:00:00Z"))
	r = results(t, out)[0]
	if resultField(t, r, "status") != "conflict" {
		t.Fatalf("correction resurrection = %v", r)
	}
	if rec, ok := resultField(t, r, "serverRecord").(map[string]any); ok {
		if rec["deleted"] != true {
			t.Fatalf("tombstone record not deleted: %v", rec)
		}
	}

	// Pull round-trips the correction record shape (correctionXxx keys).
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	sawCorrection := false
	for _, c := range changes {
		m := c.(map[string]any)
		if m["entityType"] != "transcript_correction" || m["operation"] != "upsert" {
			continue
		}
		sawCorrection = true
		rec := m["record"].(map[string]any)
		if rec["correctionRussian"] != "Привет, мир!" {
			t.Fatalf("correction record russian wrong: %v", rec)
		}
		if _, present := rec["correctionChinese"]; present {
			t.Fatalf("nil chinese must be omitted: %v", rec)
		}
	}
	if !sawCorrection {
		t.Fatalf("no transcript_correction record in pull")
	}

	// Status counts the corrections.
	resp, raw := e.get("/v1/sync/status", u.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", resp.StatusCode, raw)
	}
	var st struct {
		TranscriptCorrectionCount int `json:"transcriptCorrectionCount"`
	}
	decode(t, raw, &st)
	// The correction is tombstoned → live count 0.
	if st.TranscriptCorrectionCount != 0 {
		t.Fatalf("correction count = %d, want 0 (tombstoned)", st.TranscriptCorrectionCount)
	}
}

// §Entry time source: an explicit "audio" marker round-trips; absent keeps
// the stored (legacy) default.
func TestSyncEntryTimeSource(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("timesource@example.com", "correct-horse-battery-9")

	sessID, entryID := uuid.New().String(), uuid.New().String()
	audio := entryOp(uuid.New().String(), sessID, entryID, 1, "Точно")
	audio["payload"].(map[string]any)["timeSource"] = "audio"
	_, _, out := e.push(t, u.AccessToken, sessionOp(uuid.New().String(), sessID, 0, "x"), audio)
	if resultField(t, results(t, out)[1], "status") != "accepted" {
		t.Fatalf("audio entry push failed: %v", out)
	}

	legacyID := uuid.New().String()
	_, _, out = e.push(t, u.AccessToken, entryOp(uuid.New().String(), sessID, legacyID, 2, "Старая"))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("legacy entry push failed: %v", out)
	}

	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	for _, c := range changes {
		m := c.(map[string]any)
		if m["entityType"] != "entry" {
			continue
		}
		rec := m["record"].(map[string]any)
		switch rec["id"] {
		case entryID:
			if rec["timeSource"] != "audio" {
				t.Fatalf("audio marker lost: %v", rec)
			}
		case legacyID:
			if rec["timeSource"] != "legacy" {
				t.Fatalf("legacy default wrong: %v", rec)
			}
		}
	}
}
