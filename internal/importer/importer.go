// Package importer performs the one-shot migration of a Python-service
// SQLite database into the Go server's PostgreSQL schema.
//
// Contract:
//   - the source file is opened READ-ONLY (never written, never moved);
//   - the default mode is a DRY RUN: nothing touches PostgreSQL;
//   - --apply runs inside ONE transaction — any failure rolls everything
//     back (safe re-run);
//   - a non-empty target is refused unless --allow-non-empty;
//   - UUIDs, user relations, server_version, change_sequence, tombstones
//     and the idempotency ledger are preserved verbatim;
//   - sequences are reset to max(id) after the copy;
//   - nothing resembling credentials is imported: the Python schema has no
//     password columns, and no API keys / logs / model paths exist in it;
//     users arrive with NO password (they sign in with Apple/dev as before,
//     or set a password through the reset flow afterwards);
//   - the JSON report carries counts only — no transcript text, no
//     credentials, no emails.
package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"livetranslate/server/db"
	"livetranslate/server/internal/sqlitereader"
	"livetranslate/server/internal/store"
)

// Options configures one import run.
type Options struct {
	SourcePath    string
	TargetDSN     string
	Apply         bool
	AllowNonEmpty bool
	ReportPath    string
}

// Report is the machine-readable summary written to --report.
type Report struct {
	DryRun         bool           `json:"dryRun"`
	Source         string         `json:"source"`
	StartedAt      time.Time      `json:"startedAt"`
	DurationMs     int64          `json:"durationMs"`
	TableCounts    map[string]int `json:"tableCounts"`
	TargetEmpty    bool           `json:"targetEmpty"`
	Warnings       []string       `json:"warnings,omitempty"`
	Applied        bool           `json:"applied"`
	SequencesReset []string       `json:"sequencesReset,omitempty"`
}

// Importer runs the migration.
type Importer struct {
	opts Options
}

func New(opts Options) *Importer { return &Importer{opts: opts} }

// Run executes the import (dry-run by default). The returned report is
// always populated, regardless of dry-run or apply mode.
func (imp *Importer) Run(ctx context.Context) (*Report, error) {
	report := &Report{
		DryRun:      !imp.opts.Apply,
		Source:      imp.opts.SourcePath,
		StartedAt:   time.Now().UTC(),
		TableCounts: map[string]int{},
	}
	defer func() {
		report.DurationMs = time.Since(report.StartedAt).Milliseconds()
	}()

	// --- Source (read-only) ---------------------------------------------------
	src, err := sqlitereader.Open(imp.opts.SourcePath)
	if err != nil {
		return report, fmt.Errorf("open sqlite source: %w", err)
	}
	missingExpected := []string{}
	for _, t := range requiredTables {
		if !src.HasTable(t) {
			missingExpected = append(missingExpected, t)
		}
	}
	if len(missingExpected) > 0 {
		return report, fmt.Errorf("source is missing expected tables: %s (is this a LiveTranslate Python database?)",
			strings.Join(missingExpected, ", "))
	}
	for _, t := range importOrder {
		rows, err := src.ReadTable(t)
		if err != nil {
			return report, fmt.Errorf("read table %s: %w", t, err)
		}
		report.TableCounts[t] = len(rows)
	}

	// --- Target (migrate schema if needed, then emptiness check) ---------------
	if imp.opts.Apply {
		if err := db.Migrate(imp.opts.TargetDSN); err != nil {
			return report, fmt.Errorf("target migrations: %w", err)
		}
		st, err := store.NewDB(ctx, imp.opts.TargetDSN)
		if err != nil {
			return report, fmt.Errorf("connect target: %w", err)
		}
		empty, err := targetEmpty(ctx, st)
		if err != nil {
			st.Close()
			return report, err
		}
		report.TargetEmpty = empty
		if !empty && !imp.opts.AllowNonEmpty {
			st.Close()
			return report, fmt.Errorf(
				"target database is not empty (existing users/sessions found); re-run with --allow-non-empty to merge into it — every UUID conflict still aborts the import")
		}
		if err := imp.applyAll(ctx, st, src, report); err != nil {
			st.Close()
			return report, err // the transaction rolled back; safe to re-run
		}
		report.Applied = true
		st.Close()
	} else {
		report.Warnings = append(report.Warnings,
			"dry-run: no data was written to PostgreSQL (re-run with --apply)")
	}

	if imp.opts.ReportPath != "" {
		if err := writeReport(imp.opts.ReportPath, report); err != nil {
			return report, err
		}
	}
	return report, nil
}

// requiredTables — the Python Alembic 0001 schema.
var requiredTables = []string{
	"users", "devices", "refresh_tokens", "classroom_sessions",
	"transcript_entries", "bookmarks", "favorite_sessions",
	"sync_changes", "processed_operations",
}

// importOrder respects FK-style relationships (users first, children after;
// processed_operations last).
var importOrder = []string{
	"users", "devices", "refresh_tokens", "classroom_sessions",
	"transcript_entries", "bookmarks", "favorite_sessions",
	"sync_changes", "processed_operations",
}

func targetEmpty(ctx context.Context, st *store.DB) (bool, error) {
	var users, sessions int
	err := st.Q().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM classroom_sessions)
	`).Scan(&users, &sessions)
	if err != nil {
		return false, fmt.Errorf("target emptiness check: %w", err)
	}
	return users == 0 && sessions == 0, nil
}

// applyAll copies every table inside ONE transaction; sequences are reset
// inside the same transaction, so the final state is consistent or absent.
func (imp *Importer) applyAll(ctx context.Context, st *store.DB, src *sqlitereader.DB, report *Report) error {
	return st.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		for _, table := range importOrder {
			rows, err := src.ReadTable(table)
			if err != nil {
				return fmt.Errorf("read %s: %w", table, err)
			}
			if err := copyTable(ctx, q, table, rows, report); err != nil {
				return fmt.Errorf("copy %s: %w", table, err)
			}
		}
		// Reset the sequences the BIGSERIAL columns feed, so new inserts do
		// not collide with imported ids.
		if _, err := q.Exec(ctx, `
			SELECT setval('sync_changes_change_sequence_seq',
				coalesce((SELECT max(change_sequence) FROM sync_changes), 1))`); err != nil {
			return fmt.Errorf("reset sync_changes sequence: %w", err)
		}
		if _, err := q.Exec(ctx, `
			SELECT setval('processed_operations_id_seq',
				coalesce((SELECT max(id) FROM processed_operations), 1))`); err != nil {
			return fmt.Errorf("reset processed_operations sequence: %w", err)
		}
		report.SequencesReset = []string{"sync_changes_change_sequence_seq", "processed_operations_id_seq"}
		return nil
	})
}

// normalizeUUID accepts both the dashed 36-char form (PostgreSQL) and the
// 32-hex form SQLAlchemy's Uuid type writes on SQLite.
func normalizeUUID(raw string) (uuid.UUID, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	switch len(raw) {
	case 36:
		return uuid.Parse(raw)
	case 32:
		dashed := raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
		return uuid.Parse(dashed)
	default:
		return uuid.Nil, fmt.Errorf("not a UUID column value: %q", raw)
	}
}

func rowUUID(row sqlitereader.Row, col string) (uuid.UUID, error) {
	v, ok := row.Col(col)
	if !ok || v.IsNull() {
		return uuid.Nil, fmt.Errorf("missing required column %s", col)
	}
	return normalizeUUID(v.AsString())
}

func rowTimePtr(row sqlitereader.Row, col string) (*time.Time, error) {
	v, ok := row.Col(col)
	if !ok || v.IsNull() {
		return nil, nil
	}
	t, err := v.AsTime()
	if err != nil {
		return nil, fmt.Errorf("column %s: %w", col, err)
	}
	return &t, nil
}

func rowTime(row sqlitereader.Row, col string) (time.Time, error) {
	t, err := rowTimePtr(row, col)
	if err != nil {
		return time.Time{}, err
	}
	if t == nil {
		return time.Time{}, fmt.Errorf("column %s: required timestamp is NULL", col)
	}
	return *t, nil
}

func rowString(row sqlitereader.Row, col string) string {
	v, ok := row.Col(col)
	if !ok || v.IsNull() {
		return ""
	}
	return v.AsString()
}

func rowIntPtr(row sqlitereader.Row, col string) (*int, error) {
	v, ok := row.Col(col)
	if !ok || v.IsNull() {
		return nil, nil
	}
	n, err := v.AsInt()
	if err != nil {
		return nil, fmt.Errorf("column %s: %w", col, err)
	}
	ni := int(n)
	return &ni, nil
}

func rowInt(row sqlitereader.Row, col string, fallback int) (int, error) {
	p, err := rowIntPtr(row, col)
	if err != nil {
		return 0, err
	}
	if p == nil {
		return fallback, nil
	}
	return *p, nil
}

func rowFloat(row sqlitereader.Row, col string, fallback float64) (float64, error) {
	v, ok := row.Col(col)
	if !ok || v.IsNull() {
		return fallback, nil
	}
	switch v.Type {
	case sqlitereader.TypeReal:
		return v.Real, nil
	case sqlitereader.TypeInt:
		return float64(v.Int), nil
	default:
		return 0, fmt.Errorf("column %s: not numeric", col)
	}
}

func writeReport(path string, report *Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
