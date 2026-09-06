package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// §Errand cases (00015, 办事事项): case + checklist-item lifecycle —
// insert, independent-item concurrent updates, same-version replay,
// modified_at merge, terminal-status stickiness, cascade delete,
// record round-trip, status counts, wire hygiene.
func TestSyncErrandCaseLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("errand@example.com", "quixotic-otter-jamboree-5")

	caseID := uuid.New().String()
	_, _, out := e.push(t, u.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        caseID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"title":                  "宿舍登记",
			"errandScene":            "dorm",
			"errandStatus":           "preparing",
			"errandPurpose":          "办理宿舍入住登记并领取门禁卡",
			"errandNote":             "带翻译件",
			"errandTimezone":         "Europe/Moscow",
			"errandLocation":         "宿舍管理处 203 室",
			"errandContact":          "管理处 +7 495 000-00-00",
			"errandExpectedResultAt": "2026-09-20T09:00:00Z",
			"errandPinned":           true,
			"errandHasLocalSources":  true,
		},
	})
	r := results(t, out)[0]
	if resultField(t, r, "status") != "accepted" {
		t.Fatalf("case push failed: %v", r)
	}
	if v := int(resultField(t, r, "serverVersion").(float64)); v != 1 {
		t.Fatalf("case serverVersion = %d, want 1", v)
	}

	// Checklist items: two documents and one appointment. Items are
	// independent rows — this is the concurrency unit under test.
	itemOp := func(id string, base int, title, kind, status string, extra map[string]any) map[string]any {
		payload := map[string]any{
			"caseId":             caseID,
			"title":              title,
			"errandItemKind":     kind,
			"errandItemStatus":   status,
			"errandItemSequence": 1,
		}
		for k, v := range extra {
			payload[k] = v
		}
		return map[string]any{
			"operationId":     uuid.New().String(),
			"entityType":      "errand_case_item",
			"entityId":        id,
			"operation":       "upsert",
			"baseVersion":     base,
			"clientUpdatedAt": "2026-09-06T10:00:01Z",
			"payload":         payload,
		}
	}
	doc1 := uuid.New().String()
	doc2 := uuid.New().String()
	appt := uuid.New().String()
	_, _, out = e.push(t, u.AccessToken,
		itemOp(doc1, 0, "护照原件", "requiredDocument", "pending", map[string]any{
			"errandItemDetail": "原件 + 复印件",
		}),
		itemOp(doc2, 0, "落地签复印件", "requiredDocument", "pending", nil),
		itemOp(appt, 0, "宿舍登记预约", "appointment", "pending", map[string]any{
			"errandItemDueAt":          "2026-09-10T09:00:00Z",
			"errandItemDateText":       "周四上午",
			"errandItemDateIsRelative": true,
			"errandItemOrigin":         "ai",
			"errandItemConfirmed":      true,
			"errandItemFeeText":        "登记费 200₽",
			"errandItemFeeAmount":      200.0,
			"errandItemFeeCurrency":    "RUB",
		}),
	)
	for i, res := range results(t, out) {
		if resultField(t, res, "status") != "accepted" {
			t.Fatalf("item %d push failed: %v", i, res)
		}
	}

	// Concurrent edits of DIFFERENT items on the same base version of the
	// case: both accepted, neither overwrites the other (independent rows
	// with independent server_versions).
	_, _, out = e.push(t, u.AccessToken,
		itemOp(doc1, 1, "护照原件", "requiredDocument", "done", map[string]any{
			"errandItemCompletedAt": "2026-09-07T10:00:00Z",
		}),
		itemOp(doc2, 1, "落地签复印件（三份）", "requiredDocument", "pending", map[string]any{
			"errandItemDetail":     "三份复印件",
			"errandItemModifiedAt": "2026-09-07T10:00:00Z",
		}),
	)
	for i, res := range results(t, out) {
		if resultField(t, res, "status") != "accepted" {
			t.Fatalf("concurrent item edit %d failed: %v", i, res)
		}
	}
	var title string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT title FROM errand_case_items WHERE id = $1`, doc1).Scan(&title)
	if title != "护照原件" {
		t.Fatalf("doc1 title = %q, want 护照原件 (independent rows must not cross-edit)", title)
	}
	var detail string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT detail FROM errand_case_items WHERE id = $1`, doc2).Scan(&detail)
	if detail != "三份复印件" {
		t.Fatalf("doc2 detail = %q, want 三份复印件", detail)
	}

	// Same-version replay with an OLDER modifiedAt: the stored (newer)
	// text wins — a stale device replay never reverts a user edit.
	stale := itemOp(doc2, 2, "落地签复印件", "requiredDocument", "pending", map[string]any{
		"errandItemDetail":     "旧说明",
		"errandItemModifiedAt": "2026-09-06T08:00:00Z",
	})
	_, _, out = e.push(t, u.AccessToken, stale)
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("stale replay rejected: %v", out)
	}
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT detail FROM errand_case_items WHERE id = $1`, doc2).Scan(&detail)
	if detail != "三份复印件" {
		t.Fatalf("stale replay overwrote newer detail: %q", detail)
	}

	// Terminal-status stickiness: an un-stamped pending push (old client
	// semantics, no modifiedAt) never reopens a done item.
	reopen := itemOp(doc1, 2, "护照原件", "requiredDocument", "pending", nil)
	_, _, out = e.push(t, u.AccessToken, reopen)
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("reopen push failed: %v", out)
	}
	var status string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT status FROM errand_case_items WHERE id = $1`, doc1).Scan(&status)
	if status != "done" {
		t.Fatalf("stale pending push reopened done item: %q", status)
	}

	// Future base version → rejected (never a silent accept).
	ahead := itemOp(doc2, 99, "x", "action", "pending", nil)
	_, _, out = e.push(t, u.AccessToken, ahead)
	if resultField(t, results(t, out)[0], "status") != "rejected" {
		t.Fatalf("future base accepted: %v", out)
	}

	// Validation rejects: bad scene, bad status, bad kind, bad timezone,
	// missing caseId, over-long currency.
	bad := func(mutate func(map[string]any)) map[string]any {
		op := itemOp(uuid.New().String(), 0, "x", "action", "pending", nil)
		mutate(op["payload"].(map[string]any))
		return op
	}
	badCase := map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        uuid.New().String(),
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"errandScene":  "courtroom",
			"errandStatus": "preparing",
		},
	}
	badCaseStatus := map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        uuid.New().String(),
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"errandStatus": "finished",
		},
	}
	badTZ := map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        uuid.New().String(),
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"errandTimezone": "Mars/Olympus",
		},
	}
	ops := []map[string]any{
		badCase, badCaseStatus, badTZ,
		bad(func(p map[string]any) { p["errandItemKind"] = "snack" }),
		bad(func(p map[string]any) { p["errandItemOrigin"] = "psychic" }),
		bad(func(p map[string]any) { delete(p, "caseId") }),
		bad(func(p map[string]any) { p["errandItemFeeCurrency"] = "TOOLONGCUR" }),
	}
	_, _, out = e.push(t, u.AccessToken, ops...)
	for i, res := range results(t, out) {
		if resultField(t, res, "status") != "rejected" {
			t.Fatalf("validation case %d accepted: %v", i, res)
		}
	}

	// Same-version case merge: absent pointers keep, purpose '' clears
	// (full desired state), title never blanks.
	_, _, out = e.push(t, u.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        caseID,
		"operation":       "upsert",
		"baseVersion":     1,
		"clientUpdatedAt": "2026-09-08T10:00:00Z",
		"payload": map[string]any{
			"errandStatus":   "scheduled",
			"errandPurpose":  "",
			"errandTimezone": "Europe/Moscow",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("case merge push failed: %v", out)
	}
	var title2, purpose, tz string
	var hasLocal bool
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT title, purpose, timezone, has_local_sources FROM errand_cases WHERE id = $1`, caseID,
	).Scan(&title2, &purpose, &tz, &hasLocal)
	if title2 != "宿舍登记" {
		t.Fatalf("case merge blanked title: %q", title2)
	}
	if purpose != "" {
		t.Fatalf("case merge did not clear purpose (full desired state): %q", purpose)
	}
	if tz != "Europe/Moscow" {
		t.Fatalf("case merge lost timezone: %q", tz)
	}
	if !hasLocal {
		t.Fatalf("case merge lost hasLocalSources (absent pointer must keep)")
	}

	// Case delete cascades tombstones to its items.
	_, _, out = e.push(t, u.AccessToken, deleteOp(uuid.New().String(), "errand_case", caseID, 2))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("case delete failed: %v", out)
	}
	for _, id := range []string{doc1, doc2, appt} {
		var deleted *string
		_ = testDB.Pool.QueryRow(t.Context(),
			`SELECT deleted_at::text FROM errand_case_items WHERE id = $1`, id).Scan(&deleted)
		if deleted == nil {
			t.Fatalf("item %s survived case delete", id)
		}
	}

	// Resurrection attempt against the tombstoned case → delete-wins.
	resurrect := map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        caseID,
		"operation":       "upsert",
		"baseVersion":     99,
		"clientUpdatedAt": "2026-09-06T11:00:00Z",
		"payload":         map[string]any{},
	}
	_, _, out = e.push(t, u.AccessToken, resurrect)
	r = results(t, out)[0]
	if resultField(t, r, "status") != "conflict" {
		t.Fatalf("case resurrection = %v", r)
	}
	if rec, ok := resultField(t, r, "serverRecord").(map[string]any); ok {
		if rec["deleted"] != true {
			t.Fatalf("tombstone record not deleted: %v", rec)
		}
	}

	// Pull round-trips the record shapes (errandXxx / errandItemXxx
	// keys), including cascade delete changes for the items.
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	sawCaseUpsert, sawCaseDelete, sawItemUpsert, sawItemDelete := false, false, false, false
	for _, c := range changes {
		m := c.(map[string]any)
		switch m["entityType"] {
		case "errand_case":
			if m["operation"] == "delete" {
				sawCaseDelete = true
				continue
			}
			sawCaseUpsert = true
			rec := m["record"].(map[string]any)
			if rec["errandScene"] != "dorm" {
				t.Fatalf("case record scene wrong: %v", rec)
			}
			// The content-free local-source flag rides; the links never do.
			if rec["errandHasLocalSources"] != true {
				t.Fatalf("case record hasLocalSources wrong: %v", rec)
			}
		case "errand_case_item":
			if m["operation"] == "delete" {
				sawItemDelete = true
				continue
			}
			sawItemUpsert = true
			rec := m["record"].(map[string]any)
			if rec["caseId"] != caseID {
				t.Fatalf("item record case wrong: %v", rec)
			}
			if rec["errandItemFeeCurrency"] != "RUB" {
				t.Fatalf("item record currency wrong: %v", rec)
			}
		}
	}
	if !sawCaseUpsert || !sawCaseDelete || !sawItemUpsert || !sawItemDelete {
		t.Fatalf("pull missing changes: case=%v/%v item=%v/%v",
			sawCaseUpsert, sawCaseDelete, sawItemUpsert, sawItemDelete)
	}

	// Status counts live rows only (everything tombstoned by now).
	resp, raw := e.get("/v1/sync/status", u.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", resp.StatusCode, raw)
	}
	var st struct {
		ErrandCaseCount     int `json:"errandCaseCount"`
		ErrandCaseItemCount int `json:"errandCaseItemCount"`
	}
	decode(t, raw, &st)
	if st.ErrandCaseCount != 0 || st.ErrandCaseItemCount != 0 {
		t.Fatalf("errand counts = %d/%d, want 0/0 (tombstoned)",
			st.ErrandCaseCount, st.ErrandCaseItemCount)
	}
}

// §Errand wire hygiene: local-source payloads (document names, page
// labels, snippets) are NOT wire fields — an old or hostile client
// sending them is rejected at the DTO boundary (DisallowUnknownFields →
// 400, the whole batch refused). The wire only carries the content-free
// errandHasLocalSources flag.
func TestSyncErrandWireHygiene(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("errand-hygiene@example.com", "quixotic-otter-jamboree-5")

	leak := func(extra map[string]any) map[string]any {
		payload := map[string]any{
			"title":        "宿舍登记",
			"errandScene":  "dorm",
			"errandStatus": "preparing",
		}
		for k, v := range extra {
			payload[k] = v
		}
		return map[string]any{
			"operationId":     uuid.New().String(),
			"entityType":      "errand_case",
			"entityId":        uuid.New().String(),
			"operation":       "upsert",
			"baseVersion":     0,
			"clientUpdatedAt": "2026-09-06T10:00:00Z",
			"payload":         payload,
		}
	}
	leaks := []map[string]any{
		leak(map[string]any{"localSources": []any{map[string]any{"documentName": "护照.pdf", "pageNumber": 1}}}),
		leak(map[string]any{"errandSourceLinks": "[{\"documentID\":\"…\"}]"}),
		leak(map[string]any{"errandItemSnippet": "护照 RU 1234 567890"}),
	}
	for i, op := range leaks {
		resp, _, _ := e.push(t, u.AccessToken, op)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("local-source leak %d: status = %d, want 400 (DTO must refuse unknown source fields)", i, resp.StatusCode)
		}
	}
	// Nothing was written.
	var n int
	_ = testDB.Pool.QueryRow(t.Context(), `SELECT count(*) FROM errand_cases`).Scan(&n)
	if n != 0 {
		t.Fatalf("leaked rows persisted: %d", n)
	}
}

// §Errand user isolation: another user's push of someone else's case or
// item id must not touch the owner's row (id collision → IntegrityError
// → 500, the Python parity convention).
func TestSyncErrandUserIsolation(t *testing.T) {
	e := newEnv(t, nil)
	owner := e.registerAndVerify("errand-owner@example.com", "quixotic-otter-jamboree-5")
	other := e.registerAndVerify("errand-other@example.com", "quixotic-otter-jamboree-6")

	caseID := uuid.New().String()
	_, _, out := e.push(t, owner.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        caseID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"title":        "主人的事项",
			"errandScene":  "dorm",
			"errandStatus": "preparing",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("owner case push failed: %v", out)
	}

	// The other user pushes the SAME case id: the INSERT collides on the
	// global primary key → 500, owner's row untouched.
	resp, _, _ := e.push(t, other.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case",
		"entityId":        caseID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"title":        "攻击者的事项",
			"errandScene":  "bank",
			"errandStatus": "completed",
		},
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("cross-user id collision = %d, want 500 (Python parity)", resp.StatusCode)
	}
	var title string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT title FROM errand_cases WHERE id = $1`, caseID).Scan(&title)
	if title != "主人的事项" {
		t.Fatalf("owner row was touched: %q", title)
	}

	// The other user's item may reference the owner's case id: rows are
	// user-scoped, the dangling reference is stored but never lets the
	// other user read or cascade the owner's data.
	itemID := uuid.New().String()
	_, _, out = e.push(t, other.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "errand_case_item",
		"entityId":        itemID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-06T10:00:00Z",
		"payload": map[string]any{
			"caseId":           caseID,
			"title":            "别人的清单项",
			"errandItemKind":   "action",
			"errandItemStatus": "pending",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("other user item push failed: %v", out)
	}
	// Deleting the owner's case must NOT tombstone the other user's item
	// (cascade filters by user_id).
	_, _, out = e.push(t, owner.AccessToken, deleteOp(uuid.New().String(), "errand_case", caseID, 1))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("owner case delete failed: %v", out)
	}
	var otherItemDeleted *string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT deleted_at::text FROM errand_case_items WHERE id = $1`, itemID).Scan(&otherItemDeleted)
	if otherItemDeleted != nil {
		t.Fatalf("owner's cascade tombstoned the other user's item")
	}
}
