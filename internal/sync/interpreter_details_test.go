package sync

import (
	"encoding/json"
	"strings"
	"testing"
)

// Round 17: interpreter turn details must never persist file-source
// labels ("<documentName> · 第N页") — they leak the on-device file name
// of potentially sensitive documents (passport scans, bank statements).
func TestSanitizeInterpreterDetails(t *testing.T) {
	// nil passes through.
	if got := sanitizeInterpreterDetails(nil); got != nil {
		t.Fatalf("nil details = %v, want nil", got)
	}

	// Details without labels are byte-identical (idempotent no-op).
	clean := `{"intentSummary":"询问材料","keywords":["привет 你好"],"detailsAvailable":true}`
	got := sanitizeInterpreterDetails(&clean)
	if got == nil || *got != clean {
		t.Fatalf("clean details rewritten: %v", got)
	}

	// Labels are stripped; hasLocalSources is set; other keywords stay.
	dirty := `{"intentSummary":"文件分析","keywords":["Сумма: 15000₽","护照_扫描.pdf · 第1页"],"detailsAvailable":true}`
	got = sanitizeInterpreterDetails(&dirty)
	if got == nil {
		t.Fatal("scrubbed details = nil")
	}
	doc := decodeDetails(t, *got)
	keywords, _ := doc["keywords"].([]any)
	if len(keywords) != 1 || keywords[0] != "Сумма: 15000₽" {
		t.Fatalf("keywords after scrub = %v, want the non-source fact only", keywords)
	}
	if has, _ := doc["hasLocalSources"].(bool); !has {
		t.Fatalf("hasLocalSources = %v, want true", doc["hasLocalSources"])
	}
	// The file name must be gone entirely.
	if strings.Contains(*got, "护照_扫描.pdf") {
		t.Fatalf("file name survived the scrub: %s", *got)
	}

	// All-labels keywords: the key is removed, not left empty.
	allLabels := `{"keywords":["登记表.pdf · 第2页","通知.jpg · 第 3 页"]}`
	got = sanitizeInterpreterDetails(&allLabels)
	doc = decodeDetails(t, *got)
	if _, still := doc["keywords"]; still {
		t.Fatalf("keywords key should be dropped when empty: %s", *got)
	}
	if has, _ := doc["hasLocalSources"].(bool); !has {
		t.Fatalf("hasLocalSources = %v, want true", doc["hasLocalSources"])
	}

	// Scrubbing an already-scrubbed value changes nothing (idempotent).
	once := sanitizeInterpreterDetails(&dirty)
	twice := sanitizeInterpreterDetails(once)
	if *once != *twice {
		t.Fatalf("scrub is not idempotent:\nfirst:  %s\nsecond: %s", *once, *twice)
	}

	// Non-object JSON passes through untouched (validJSONPayload already
	// gated syntax; the scrub never mangles what it does not understand).
	arrayJSON := `[1,2,3]`
	got = sanitizeInterpreterDetails(&arrayJSON)
	if *got != arrayJSON {
		t.Fatalf("non-object details rewritten: %s", *got)
	}

	// An explicit hasLocalSources=false is corrected when labels existed:
	// the labels prove sources exist.
	contradictory := `{"keywords":["签证.pdf · 第1页"],"hasLocalSources":false}`
	got = sanitizeInterpreterDetails(&contradictory)
	doc = decodeDetails(t, *got)
	if has, _ := doc["hasLocalSources"].(bool); !has {
		t.Fatalf("hasLocalSources = %v, want true after removing labels", doc["hasLocalSources"])
	}
}

// decodeDetails parses details JSON into a FRESH map — json.Unmarshal
// merges into a non-nil map, so reusing one would leak earlier keys into
// later assertions.
func decodeDetails(t *testing.T, raw string) map[string]any {
	t.Helper()
	doc := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("details not valid JSON (%s): %v", raw, err)
	}
	return doc
}
