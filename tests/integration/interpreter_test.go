package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// §Interpreter (00014): the 随身翻译 lifecycle — conversation + turns push,
// turn modified_at merge, cascade delete, record round-trip, status counts.
func TestSyncInterpreterLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	// Password deliberately NOT similar to the email local-part.
	u := e.registerAndVerify("interpreter@example.com", "quixotic-otter-jamboree-5")

	convID := uuid.New().String()
	_, _, out := e.push(t, u.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        convID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:00Z",
		"payload": map[string]any{
			"title":                  "宿舍办理 · 9月5日",
			"interpreterScene":       "dorm",
			"interpreterContextNote": "我是莫斯科国立大学留学生",
			"interpreterStatus":      "saved",
			"interpreterStartedAt":   "2026-09-05T10:00:00Z",
		},
	})
	r := results(t, out)[0]
	if resultField(t, r, "status") != "accepted" {
		t.Fatalf("conversation push failed: %v", r)
	}
	if v := int(resultField(t, r, "serverVersion").(float64)); v != 1 {
		t.Fatalf("conversation serverVersion = %d, want 1", v)
	}

	// Turn upserts (one counterpart ru2zh, one user zh2ru).
	turnOp := func(id string, seq int, speaker, direction string) map[string]any {
		return map[string]any{
			"operationId":     uuid.New().String(),
			"entityType":      "interpreter_turn",
			"entityId":        id,
			"operation":       "upsert",
			"baseVersion":     0,
			"clientUpdatedAt": "2026-09-05T10:00:01Z",
			"payload": map[string]any{
				"conversationId":      convID,
				"turnSpeaker":         speaker,
				"turnDirection":       direction,
				"turnInputMethod":     "audio",
				"turnSequence":        seq,
				"turnSourceText":      "У вас есть копия паспорта?",
				"turnPlainRussian":    "У вас есть копия паспорта?",
				"turnStressedRussian": "У вас есть ко́пия па́спорта?",
				"turnChineseText":     "您有护照复印件吗？",
				"turnBackTranslation": "",
				"turnModifiedAt":      "2026-09-05T10:00:01Z",
			},
		}
	}
	turn1 := uuid.New().String()
	turn2 := uuid.New().String()
	_, _, out = e.push(t, u.AccessToken,
		turnOp(turn1, 1, "counterpart", "ru2zh"),
		turnOp(turn2, 2, "user", "zh2ru"),
	)
	if resultField(t, results(t, out)[0], "status") != "accepted" ||
		resultField(t, results(t, out)[1], "status") != "accepted" {
		t.Fatalf("turn pushes failed: %v", out)
	}

	// Same-version edit with an OLDER modifiedAt: the newer stored text wins.
	edit := turnOp(turn1, 1, "counterpart", "ru2zh")
	edit["baseVersion"] = 1
	edit["payload"].(map[string]any)["turnSourceText"] = "старая версия"
	edit["payload"].(map[string]any)["turnModifiedAt"] = "2026-09-05T09:00:00Z"
	_, _, out = e.push(t, u.AccessToken, edit)
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("turn edit push failed: %v", out)
	}
	var src string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT source_text FROM interpreter_turns WHERE id = $1`, turn1).Scan(&src)
	if src != "У вас есть копия паспорта?" {
		t.Fatalf("older turn edit overwrote newer text: %q", src)
	}

	// Turn without a conversation reference → rejected.
	orphan := turnOp(uuid.New().String(), 3, "counterpart", "ru2zh")
	delete(orphan["payload"].(map[string]any), "conversationId")
	_, _, out = e.push(t, u.AccessToken, orphan)
	if resultField(t, results(t, out)[0], "status") != "rejected" {
		t.Fatalf("orphan turn accepted: %v", out)
	}

	// Bad scene → rejected.
	badScene := map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        uuid.New().String(),
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:00Z",
		"payload": map[string]any{
			"interpreterScene":     "courtroom",
			"interpreterStartedAt": "2026-09-05T10:00:00Z",
		},
	}
	_, _, out = e.push(t, u.AccessToken, badScene)
	if resultField(t, results(t, out)[0], "status") != "rejected" {
		t.Fatalf("bad scene accepted: %v", out)
	}

	// Conversation delete cascades tombstones to its turns.
	_, _, out = e.push(t, u.AccessToken, deleteOp(uuid.New().String(), "interpreter_conversation", convID, 1))
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("conversation delete failed: %v", out)
	}
	for _, id := range []string{turn1, turn2} {
		var deleted *string
		_ = testDB.Pool.QueryRow(t.Context(),
			`SELECT deleted_at::text FROM interpreter_turns WHERE id = $1`, id).Scan(&deleted)
		if deleted == nil {
			t.Fatalf("turn %s survived conversation delete", id)
		}
	}

	// Resurrection attempt against the tombstoned conversation → delete-wins.
	resurrect := map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        convID,
		"operation":       "upsert",
		"baseVersion":     99,
		"clientUpdatedAt": "2026-09-05T11:00:00Z",
		"payload": map[string]any{
			"interpreterStartedAt": "2026-09-05T10:00:00Z",
		},
	}
	_, _, out = e.push(t, u.AccessToken, resurrect)
	r = results(t, out)[0]
	if resultField(t, r, "status") != "conflict" {
		t.Fatalf("conversation resurrection = %v", r)
	}
	if rec, ok := resultField(t, r, "serverRecord").(map[string]any); ok {
		if rec["deleted"] != true {
			t.Fatalf("tombstone record not deleted: %v", rec)
		}
	}

	// Pull round-trips the record shapes (interpreterXxx / turnXxx keys),
	// including the cascade delete changes for the turns.
	changes, _, _ := e.pull(t, u.AccessToken, 0, 0)
	sawConvUpsert, sawConvDelete, sawTurnDelete, sawTurnUpsert := false, false, false, false
	for _, c := range changes {
		m := c.(map[string]any)
		switch m["entityType"] {
		case "interpreter_conversation":
			if m["operation"] == "delete" {
				sawConvDelete = true
				continue
			}
			sawConvUpsert = true
			rec := m["record"].(map[string]any)
			if rec["title"] != "宿舍办理 · 9月5日" {
				t.Fatalf("conversation record title wrong: %v", rec)
			}
			if rec["interpreterScene"] != "dorm" {
				t.Fatalf("conversation record scene wrong: %v", rec)
			}
			if rec["interpreterContextNote"] != "我是莫斯科国立大学留学生" {
				t.Fatalf("conversation record context note wrong: %v", rec)
			}
		case "interpreter_turn":
			if m["operation"] == "delete" {
				sawTurnDelete = true
				continue
			}
			sawTurnUpsert = true
			rec := m["record"].(map[string]any)
			if rec["conversationId"] != convID {
				t.Fatalf("turn record conversation wrong: %v", rec)
			}
			if rec["turnStressedRussian"] != "У вас есть ко́пия па́спорта?" {
				t.Fatalf("turn record stressed russian wrong: %v", rec)
			}
		}
	}
	if !sawConvUpsert || !sawConvDelete || !sawTurnUpsert || !sawTurnDelete {
		t.Fatalf("pull missing changes: conv=%v/%v turn=%v/%v",
			sawConvUpsert, sawConvDelete, sawTurnUpsert, sawTurnDelete)
	}

	// Status counts live rows only (everything tombstoned by now).
	resp, raw := e.get("/v1/sync/status", u.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d %s", resp.StatusCode, raw)
	}
	var st struct {
		InterpreterConversationCount int `json:"interpreterConversationCount"`
		InterpreterTurnCount         int `json:"interpreterTurnCount"`
	}
	decode(t, raw, &st)
	if st.InterpreterConversationCount != 0 || st.InterpreterTurnCount != 0 {
		t.Fatalf("interpreter counts = %d/%d, want 0/0 (tombstoned)",
			st.InterpreterConversationCount, st.InterpreterTurnCount)
	}
}

// §Interpreter user isolation: another user's push of someone else's
// conversation id must not touch the owner's row.
func TestSyncInterpreterUserIsolation(t *testing.T) {
	e := newEnv(t, nil)
	alice := e.registerAndVerify("interp-alice@example.com", "correct-horse-battery-9")
	bob := e.registerAndVerify("interp-bob@example.com", "correct-horse-battery-9")

	convID := uuid.New().String()
	_, _, out := e.push(t, alice.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        convID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:00Z",
		"payload": map[string]any{
			"title":                "银行咨询 · 9月5日",
			"interpreterScene":     "bank",
			"interpreterStartedAt": "2026-09-05T10:00:00Z",
		},
	})
	if resultField(t, results(t, out)[0], "status") != "accepted" {
		t.Fatalf("alice conversation push failed: %v", out)
	}

	// Bob pushes an upsert for Alice's conversation id: the row belongs to
	// Alice (user_id), so Bob's push INSERTs nothing (PK collision) — the
	// batch fails server-side; Alice's row stays intact. Bob cannot read it
	// through pull either.
	_, _, out = e.push(t, bob.AccessToken, map[string]any{
		"operationId":     uuid.New().String(),
		"entityType":      "interpreter_conversation",
		"entityId":        convID,
		"operation":       "upsert",
		"baseVersion":     0,
		"clientUpdatedAt": "2026-09-05T10:00:00Z",
		"payload": map[string]any{
			"title":                "篡改",
			"interpreterStartedAt": "2026-09-05T10:00:00Z",
		},
	})
	var title string
	_ = testDB.Pool.QueryRow(t.Context(),
		`SELECT title FROM interpreter_conversations WHERE id = $1`, convID).Scan(&title)
	if title != "银行咨询 · 9月5日" {
		t.Fatalf("bob's push mutated alice's row: %q", title)
	}

	changes, _, _ := e.pull(t, bob.AccessToken, 0, 0)
	for _, c := range changes {
		if m := c.(map[string]any); m["entityType"] == "interpreter_conversation" {
			t.Fatalf("bob pulled alice's conversation: %v", m)
		}
	}
}
