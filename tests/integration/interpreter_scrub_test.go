package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// §Round 17 privacy: interpreter turn details must never persist
// file-source labels ("<documentName> · 第N页") — they leak the on-device
// file name of potentially sensitive documents. The server scrubs them at
// apply time on EVERY path (insert, same-version merge, stale-edit
// re-push), so old clients cannot re-introduce them after a round-17
// client cleaned a row.
func TestSyncInterpreterDetailsScrub(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("scrub@example.com", "quixotic-otter-jamboree-5")

	convID := uuid.New().String()
	_, _, out := e.push(t, u.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        convID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:00Z",
		"payload": map[string]any{
			"interpreterScene":     "migration",
			"interpreterStatus":    "saved",
			"interpreterStartedAt": "2026-09-05T10:00:00Z",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("conversation push failed: %v", out)
	}

	turnID := uuid.New().String()
	pushTurn := func(baseVersion int, details, modifiedAt string) map[string]any {
		payload := map[string]any{
			"conversationId":   convID,
			"turnSpeaker":      "counterpart",
			"turnDirection":    "ru2zh",
			"turnInputMethod":  "audio",
			"turnSequence":     1,
			"turnSourceText":   "Подайте заявление",
			"turnChineseText":  "请提交申请",
			"turnPlainRussian": "Подайте заявление",
		}
		if details != "" {
			payload["turnDetails"] = details
		}
		if modifiedAt != "" {
			payload["turnModifiedAt"] = modifiedAt
		}
		return map[string]any{
			"operationId":     uuid.New().String(),
			"entityType":      "interpreter_turn",
			"entityId":        turnID,
			"operation":       "upsert",
			"baseVersion":     baseVersion,
			"clientUpdatedAt": "2026-09-05T10:00:01Z",
			"payload":         payload,
		}
	}

	// 1. Insert path: labels embedded in keywords are scrubbed, the
	//    non-source keyword survives, hasLocalSources is set.
	dirty := `{"intentSummary":"文件分析","keywords":["Сумма: 15000₽","护照_扫描.pdf · 第1页"],"detailsAvailable":true}`
	_, _, out = e.push(t, u.AccessToken, pushTurn(0, dirty, "2026-09-05T10:00:02Z"))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("dirty turn push failed: %v", out)
	}
	assertScrubbedDetails(t, storedInterpreterDetails(t, turnID))

	// 2. Pull returns the scrubbed value — another device never sees the
	//    file name.
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	sawTurn := false
	for _, c := range changes {
		m := c.(map[string]any)
		if m["entityType"] != "interpreter_turn" || m["operation"] == "delete" {
			continue
		}
		sawTurn = true
		rec := m["record"].(map[string]any)
		details, _ := rec["turnDetails"].(string)
		assertScrubbedDetails(t, details)
	}
	if !sawTurn {
		t.Fatal("pull did not include the turn")
	}

	// 3. Stale-edit resurrection attempt: an older modifiedAt re-push of
	//    the dirty details (the pre-round-17 value another device still
	//    holds locally). Even on the merge path the labels are scrubbed —
	//    the cleaned stored value can never be re-polluted.
	_, _, out = e.push(t, u.AccessToken, pushTurn(1, dirty, "2026-09-05T09:00:00Z"))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("stale re-push failed: %v", out)
	}
	assertScrubbedDetails(t, storedInterpreterDetails(t, turnID))
	// The stored TEXT still honors newer-wins (the stale edit must not
	// have resurrected the older source text either).
	var src string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT source_text FROM interpreter_turns WHERE id = $1`, turnID).Scan(&src)
	if src != "Подайте заявление" {
		t.Fatalf("stale edit overwrote newer text: %q", src)
	}

	// 4. Clean details round-trip through the scrub unchanged (semantic
	//    equality — the JSONB column canonicalizes key order/spacing, so
	//    the scrub must not change MEANING, and must not drop fields).
	clean := `{"intentSummary":"文件分析","keywords":["Сумма: 15000₽"],"hasLocalSources":true,"detailsAvailable":true}`
	var serverVersion int
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT server_version FROM interpreter_turns WHERE id = $1`, turnID).Scan(&serverVersion)
	_, _, out = e.push(t, u.AccessToken, pushTurn(serverVersion, clean, "2026-09-05T10:00:02Z"))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("clean re-push failed: %v", out)
	}
	stored := storedInterpreterDetails(t, turnID)
	if !jsonEqual(t, stored, clean) {
		t.Fatalf("clean details changed by scrub:\n got: %s\nwant: %s", stored, clean)
	}
}

// assertScrubbedDetails fails when details still carry a file-source label
// or lost the non-source keyword / marker.
func assertScrubbedDetails(t *testing.T, details string) {
	t.Helper()
	if strings.Contains(details, "护照_扫描.pdf") {
		t.Fatalf("file name persisted: %s", details)
	}
	if !strings.Contains(details, "Сумма: 15000₽") {
		t.Fatalf("non-source keyword dropped: %s", details)
	}
	if !strings.Contains(details, "hasLocalSources") {
		t.Fatalf("hasLocalSources not set: %s", details)
	}
}

// jsonEqual compares two JSON documents semantically.
func jsonEqual(t *testing.T, a, b string) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		t.Fatalf("invalid JSON %q: %v", a, err)
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		t.Fatalf("invalid JSON %q: %v", b, err)
	}
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	return string(aj) == string(bj)
}

func storedInterpreterDetails(t *testing.T, turnID string) string {
	t.Helper()
	var details *string
	err := testDB.Pool.QueryRow(t.Context(),
		`SELECT details::text FROM interpreter_turns WHERE id = $1`, turnID).Scan(&details)
	if err != nil {
		t.Fatalf("reading stored details: %v", err)
	}
	if details == nil {
		return ""
	}
	return *details
}

// §Round 17: the scrub must not damage non-source structured details —
// intent/keywords/uncertainties ride the wire exactly as before.
func TestSyncInterpreterDetailsScrubPreservesStructuredFields(t *testing.T) {
	e := newEnv(t, nil)
	u := e.registerAndVerify("scrub2@example.com", "quixotic-otter-jamboree-5")

	convID := uuid.New().String()
	_, _, out := e.push(t, u.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        convID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:00Z",
		"payload": map[string]any{
			"interpreterScene":     "bank",
			"interpreterStatus":    "saved",
			"interpreterStartedAt": "2026-09-05T10:00:00Z",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("conversation push failed: %v", out)
	}

	turnID := uuid.New().String()
	details := `{"intentSummary":"开户要求","keywords":["паспорт 护照"],"uncertainties":["签名栏位置"],"detailsAvailable":true,"hasLocalSources":true}`
	_, _, out = e.push(t, u.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_turn",
		"entityId":        turnID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:01Z",
		"payload": map[string]any{
			"conversationId":  convID,
			"turnSpeaker":     "counterpart",
			"turnDirection":   "ru2zh",
			"turnInputMethod": "audio",
			"turnSequence":    1,
			"turnSourceText":  "Нужен паспорт",
			"turnChineseText": "需要护照",
			"turnDetails":     details,
			"turnModifiedAt":  "2026-09-05T10:00:05Z",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("turn push failed: %v", out)
	}

	// Structured fields survive the scrub semantically.
	if got := storedInterpreterDetails(t, turnID); !jsonEqual(t, got, details) {
		t.Fatalf("structured details changed:\n got: %s\nwant: %s", got, details)
	}

	// The pull response carries them too.
	resp, raw := e.get("/v1/sync/pull?cursor=0&limit=50", u.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull: %d %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, "开户要求") || !strings.Contains(raw, "签名栏位置") {
		t.Fatalf("pull lost structured details: %s", raw)
	}
}
