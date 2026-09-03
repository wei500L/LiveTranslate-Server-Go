#!/usr/bin/env bash
# LiveTranslate — PostgreSQL backup.
#
# Takes a custom-format (pg_dump -Fc) backup of ONE explicitly named
# database into ONE explicitly named output directory. Refuses broad or
# implicit targets: no database name → abort; no output dir → abort; the
# output dir must already exist and must not be inside the git working tree
# or any web-served directory.
#
# Usage:
#   ./backup.sh --database livetranslate --output-dir /var/backups/livetranslate
#   ./backup.sh --database livetranslate --output-dir ... --retention-days 14
#
# Required environment (or pgpass):
#   PGHOST / PGPORT / PGUSER — connection parameters
#   PGPASSWORD              — or rely on ~/.pgpass
#
# Exit codes: 0 ok · 1 usage error · 2 runtime failure.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: backup.sh --database NAME --output-dir DIR [--retention-days N]

  --database NAME        database to back up (required, explicit)
  --output-dir DIR       existing directory for backup files (required)
  --retention-days N     delete backups older than N days (default: 14)
EOF
  exit 1
}

DATABASE=""
OUTPUT_DIR=""
RETENTION_DAYS=14

while [[ $# -gt 0 ]]; do
  case "$1" in
    --database) DATABASE="${2:-}"; shift 2 ;;
    --output-dir) OUTPUT_DIR="${2:-}"; shift 2 ;;
    --retention-days) RETENTION_DAYS="${2:-}"; shift 2 ;;
    *) usage ;;
  esac
done

[[ -n "$DATABASE" ]] || usage
[[ -n "$OUTPUT_DIR" ]] || usage
[[ -d "$OUTPUT_DIR" ]] || { echo "error: output dir '$OUTPUT_DIR' does not exist (create it first, with mode 0700)" >&2; exit 1; }

# Safety: refuse directories that must never hold backups.
case "$OUTPUT_DIR" in
  */.git*|*/www|*/html|*/public_html|*/var/www*)
    echo "error: refusing to write backups into '$OUTPUT_DIR' (git tree or web-served path)" >&2
    exit 1 ;;
esac

# The dir should be private; warn (don't fail) when it is not.
PERMS=$(stat -c '%a' "$OUTPUT_DIR" 2>/dev/null || stat -f '%Lp' "$OUTPUT_DIR")
if [[ "$PERMS" != "700" && "$PERMS" != "750" && "$PERMS" != "770" ]]; then
  echo "warning: output dir permissions are $PERMS — backups contain user data; chmod 0700 is recommended" >&2
fi

command -v pg_dump >/dev/null || { echo "error: pg_dump not found" >&2; exit 2; }

STAMP=$(date -u +%Y%m%dT%H%M%SZ)
FILE="$OUTPUT_DIR/livetranslate-$DATABASE-$STAMP.dump"

echo "backing up database '$DATABASE' → $FILE"
if ! pg_dump --format=custom --no-owner --no-privileges --dbname="$DATABASE" --file="$FILE"; then
  rm -f "$FILE"
  echo "error: pg_dump failed (partial file removed)" >&2
  exit 2
fi

chmod 0600 "$FILE"
SIZE=$(du -h "$FILE" | cut -f1)
echo "ok: $FILE ($SIZE)"

# Verification marker: a backup that cannot even be listed is not a backup.
if ! pg_restore --list "$FILE" >/dev/null 2>&1; then
  echo "error: backup fails pg_restore --list — treat it as INVALID and investigate" >&2
  exit 2
fi

# Retention: prune by mtime.
if [[ "$RETENTION_DAYS" -gt 0 ]]; then
  find "$OUTPUT_DIR" -name "livetranslate-$DATABASE-*.dump" -type f -mtime +"$RETENTION_DAYS" -delete
  echo "retention: removed backups older than $RETENTION_DAYS days"
fi

cat >&2 <<'EOF'

Encryption-at-rest suggestion:
  gpg --symmetric --cipher-algo AES256 --output "$FILE.gpg" "$FILE" && rm "$FILE"
(or store the directory on an encrypted volume / encrypted object storage;
 see deploy/BACKUP.md).
EOF
