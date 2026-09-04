// Package importer performs the one-shot migration of a Python-service
// SQLite database into the Go server's PostgreSQL schema.
//
// Source access goes through the mature, maintained pure-Go SQLite driver
// (modernc.org/sqlite) opened in mode=ro — a SQLite-level read-only
// boundary, not a hand-rolled file parser. WAL-mode sources are REFUSED
// with instructions (a raw read of a live WAL database can miss committed
// frames; checkpoint or copy the file first).
//
// Contract:
//   - the default mode is a DRY RUN: nothing touches PostgreSQL;
//   - --apply streams the source tables inside ONE transaction — any
//     failure rolls everything back (safe re-run);
//   - a non-empty target is refused unless --allow-non-empty (existing
//     ids are then SKIPPED, the target row wins);
//   - UUIDs, user relations, server_version, change_sequence, tombstones
//     and the idempotency ledger are preserved verbatim;
//   - sequences are reset to max(id) inside the same transaction;
//   - nothing resembling credentials is imported: the Python schema has no
//     password columns, and no API keys / logs / model paths exist in it;
//     users arrive with NO password (they sign in with Apple/dev as before,
//     or set a password through the reset flow afterwards);
//   - the JSON report carries counts only — no transcript text, no
//     credentials, no emails.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	_ "modernc.org/sqlite" // registers the database/sql "sqlite" driver

	"livetranslate/server/db"
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
	SourceJournal  string         `json:"sourceJournalMode"`
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

// importOrder — the Python Alembic 0001 schema, in copy order (users
// first, children after, ledger last). checkSourceShape requires every
// table to exist in the source.
var importOrder = []string{
	"users", "devices", "refresh_tokens", "classroom_sessions",
	"transcript_entries", "bookmarks", "favorite_sessions",
	"sync_changes", "processed_operations",
}

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

	// --- Source (SQLite, read-only at the driver level) -------------------
	src, err := openSource(imp.opts.SourcePath)
	if err != nil {
		return report, err
	}
	defer src.Close()

	if err := checkSourceShape(ctx, src, report); err != nil {
		return report, err
	}

	// --- Target (migrate schema if needed, then emptiness check) ----------
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
				"target database is not empty (existing users/sessions found); re-run with --allow-non-empty to merge into it — rows whose id already exists in the target are then skipped (the target wins)")
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

// openSource opens the SQLite file through the pure-Go driver in READ-ONLY
// mode: SQLite itself refuses every write on this connection — a real
// boundary, not a convention. WAL-mode databases are refused (see checkSourceShape).
func openSource(path string) (*sql.DB, error) {
	if fi, err := os.Stat(path); err != nil {
		return nil, err
	} else if fi.IsDir() {
		return nil, fmt.Errorf("source is a directory, not a database file")
	}
	uri := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro"
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, fmt.Errorf("open sqlite source: %w", err)
	}
	// One connection: the source is only streamed once; a pool would just
	// hold extra read handles.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite source: %w", err)
	}
	return db, nil
}

// checkSourceShape validates that the source is the expected schema and
// records its journal mode. WAL is refused: a read-only connection to a
// live WAL database may read a stale snapshot (and needs the -shm/-wal
// sidecars); the operator must checkpoint or copy the file first.
func checkSourceShape(ctx context.Context, src *sql.DB, report *Report) error {
	var journal string
	if err := src.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
		return fmt.Errorf("read journal mode: %w", err)
	}
	report.SourceJournal = strings.ToLower(strings.TrimSpace(journal))
	if report.SourceJournal == "wal" {
		return fmt.Errorf(
			"source database is in WAL mode: stop the source service (or run `sqlite3 <db> 'PRAGMA wal_checkpoint(TRUNCATE);'` and copy the file) so all committed data is in the main file, then re-run")
	}
	missing := []string{}
	for _, t := range importOrder {
		var n int
		if err := src.QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, t,
		).Scan(&n); err != nil {
			return fmt.Errorf("inspect table %s: %w", t, err)
		}
		if n == 0 {
			missing = append(missing, t)
			report.TableCounts[t] = 0
			continue
		}
		if err := src.QueryRowContext(ctx, "SELECT count(*) FROM "+t).Scan(&n); err != nil {
			return fmt.Errorf("count table %s: %w", t, err)
		}
		report.TableCounts[t] = n
	}
	if len(missing) > 0 {
		return fmt.Errorf("source is missing expected tables: %s (is this a LiveTranslate Python database?)",
			strings.Join(missing, ", "))
	}
	return nil
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
// Source rows are STREAMED (rows.Next()) — never fully materialized.
func (imp *Importer) applyAll(ctx context.Context, st *store.DB, src *sql.DB, report *Report) error {
	return st.Tx(ctx, func(tx pgx.Tx) error {
		q := store.TxQ(tx)
		for _, table := range importOrder {
			copied, err := copyTable(ctx, q, src, table)
			if err != nil {
				return fmt.Errorf("copy %s: %w", table, err)
			}
			report.TableCounts[table] = copied
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

func writeReport(path string, report *Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
