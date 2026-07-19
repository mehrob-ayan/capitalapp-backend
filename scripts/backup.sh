#!/bin/sh
# Daily PostgreSQL backup with 30-day rotation.
export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

DIR="/Users/mehrob.latipov/Ayan/capital-app/backups"
mkdir -p "$DIR"
STAMP=$(date +%Y-%m-%d_%H%M)

if docker exec capital_pg pg_dump -U capital capital | gzip > "$DIR/capital_$STAMP.sql.gz"; then
	echo "$(date) backup ok: capital_$STAMP.sql.gz"
	# keep the 30 most recent, delete older
	ls -1t "$DIR"/capital_*.sql.gz | tail -n +31 | while read -r f; do rm -f "$f"; done
else
	echo "$(date) backup FAILED (is Postgres up?)" >&2
	rm -f "$DIR/capital_$STAMP.sql.gz"
fi
