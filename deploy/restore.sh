#!/usr/bin/env bash
# LiveTranslate — PostgreSQL restore.
#
# Restores a custom-format backup produced by backup.sh into ONE explicitly
# named database. The target database must either not exist or be explicitly
# confirmed with --confirm-drop (which drops and recreates it — destructive).
#
# Usage:
#   ./restore.sh --backup FILE.dump --database livetranslate
#   ./restore.sh --backup FILE.dump --database livetranslate --confirm-drop
#   ./restore.sh --list FILE.dump        # inspect a backup without restoring
#
# After any restore, follow deploy/BACKUP.md §恢复后检查 (sequence check,
# migration status, cursor sanity) before pointing the server at the data.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: restore.sh --backup FILE.dump --database NAME [--confirm-drop]
       restore.sh --list FILE.dump
EOF
  exit 1
}

BACKUP=""
DATABASE=""
CONFIRM_DROP=0
LIST_ONLY=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --backup) BACKUP="${2:-}"; shift 2 ;;
    --database) DATABASE="${2:-}"; shift 2 ;;
    --confirm-drop) CONFIRM_DROP=1; shift ;;
    --list) LIST_ONLY=1; BACKUP="${2:-}"; shift 2 ;;
    *) usage ;;
  esac
done

[[ -n "$BACKUP" ]] || usage
[[ -f "$BACKUP" ]] || { echo "error: backup file '$BACKUP' not found" >&2; exit 1; }

if [[ "$LIST_ONLY" -eq 1 ]]; then
  pg_restore --list "$BACKUP"
  exit 0
fi

[[ -n "$DATABASE" ]] || usage
command -v psql >/dev/null || { echo "error: psql not found" >&2; exit 2; }
command -v pg_restore >/dev/null || { echo "error: pg_restore not found" >&2; exit 2; }

EXISTS=$(psql --tuples-only --no-align --dbname=postgres --command \
  "SELECT 1 FROM pg_database WHERE datname = '$DATABASE'" 2>/dev/null | tr -d '[:space:]' || true)

if [[ "$EXISTS" == "1" ]]; then
  if [[ "$CONFIRM_DROP" -ne 1 ]]; then
    echo "error: database '$DATABASE' already exists. This restore would overwrite it." >&2
    echo "       Re-run with --confirm-drop to DROP and recreate it (destructive)." >&2
    exit 1
  fi
  # Refuse to drop a database that still has connections.
  psql --dbname=postgres --command \
    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$DATABASE' AND pid <> pg_backend_pid()" >/dev/null
  psql --dbname=postgres --command "DROP DATABASE \"$DATABASE\""
fi
psql --dbname=postgres --command "CREATE DATABASE \"$DATABASE\""

echo "restoring $BACKUP → database '$DATABASE'"
pg_restore --no-owner --no-privileges --dbname="$DATABASE" "$BACKUP"
echo "restore complete."

cat >&2 <<'EOF'

MUST-RUN CHECKS (see deploy/BACKUP.md §恢复后检查):
  1. migrations:      livetranslate-server migrate   (idempotent; brings the
                      restored schema up to date if the backup predates one)
  2. sequence check:  SELECT last_value FROM sync_changes_change_sequence_seq;
                      must be >= max(change_sequence) — re-import fixes this:
                      SELECT setval('sync_changes_change_sequence_seq',
                        coalesce((SELECT max(change_sequence) FROM sync_changes), 1));
  3. cursor sanity:   iOS devices pull from their stored cursor; if the
                      restored change log is OLDER than some clients' cursors,
                      those clients must sign out/in (fresh initial upload).
  4. start the server on a staging port first and /ready + a test login.
EOF
