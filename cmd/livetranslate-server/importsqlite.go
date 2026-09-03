package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"livetranslate/server/internal/config"
	"livetranslate/server/internal/importer"
)

// runImportSQLite implements:
//
//	livetranslate-server import-sqlite --source db.sqlite [--dry-run | --apply]
//	                                     [--report report.json] [--allow-non-empty]
//
// Default mode is a DRY RUN. The source file is only ever read; --apply is
// the only mode that touches PostgreSQL, and it does so in one transaction
// (failure ⇒ full rollback ⇒ safe re-run).
func runImportSQLite(args []string) error {
	fs := flag.NewFlagSet("import-sqlite", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), strings.TrimSpace(`
usage: livetranslate-server import-sqlite --source /path/to/database.sqlite [flags]

  --source PATH          SQLite database of the Python service (read-only;
                         copy it after stopping the service — a non-empty
                         -wal sidecar is refused)
  --dry-run              count and validate only (DEFAULT; no writes)
  --apply                perform the import into PostgreSQL (one transaction)
  --report PATH          write a JSON statistics report (counts only — no
                         transcript text, no credentials)
  --allow-non-empty      permit importing into a non-empty target (existing
                         ids are skipped, the target row wins)
  --database-url URL     target PostgreSQL URL (defaults to DATABASE_URL)`))
	}
	source := fs.String("source", "", "path to the source SQLite database (required)")
	dryRun := fs.Bool("dry-run", false, "count and validate only (default mode)")
	apply := fs.Bool("apply", false, "perform the import")
	reportPath := fs.String("report", "", "write a JSON report to this path")
	allowNonEmpty := fs.Bool("allow-non-empty", false, "allow importing into a non-empty target")
	databaseURL := fs.String("database-url", "", "target PostgreSQL URL (default: DATABASE_URL)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" {
		fs.Usage()
		return fmt.Errorf("--source is required")
	}
	if *dryRun && *apply {
		return fmt.Errorf("--dry-run and --apply are mutually exclusive")
	}
	dsn := *databaseURL
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		// Fall back to the config loader so a .env-style environment works
		// the same as the serve command.
		if cfg, err := config.Load(); err == nil {
			dsn = cfg.DatabaseURL
		}
	}
	if *apply && dsn == "" {
		return fmt.Errorf("--apply needs a target: pass --database-url or set DATABASE_URL")
	}

	mode := "dry-run"
	if *apply {
		mode = "apply"
	}
	fmt.Printf("import-sqlite: mode=%s source=%s\n", mode, *source)
	if *apply {
		fmt.Printf("import-sqlite: target=<%s>\n", maskDSN(dsn))
	}

	imp := importer.New(importer.Options{
		SourcePath:    *source,
		TargetDSN:     dsn,
		Apply:         *apply,
		AllowNonEmpty: *allowNonEmpty,
		ReportPath:    *reportPath,
	})
	report, err := imp.Run(context.Background())
	if err != nil {
		// The report is still valuable on failure — write it when a path
		// was given, then surface the error.
		if *reportPath != "" {
			_ = writeImportReport(*reportPath, report)
		}
		return err
	}

	out, _ := json.MarshalIndent(report, "", "  ")
	fmt.Println(string(out))
	if !*apply {
		fmt.Println("dry-run complete — nothing was written to PostgreSQL")
	} else {
		fmt.Println("import applied and committed (sequences reset)")
	}
	return nil
}

// maskDSN hides credentials in log output.
func maskDSN(dsn string) string {
	at := strings.Index(dsn, "@")
	if at < 0 || !strings.Contains(dsn[:at], ":") {
		return dsn
	}
	schemeEnd := strings.Index(dsn, "://")
	if schemeEnd < 0 {
		return dsn
	}
	return dsn[:schemeEnd+3] + "***@" + dsn[at+1:]
}

func writeImportReport(path string, r *importer.Report) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
