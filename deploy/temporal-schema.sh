#!/bin/sh
set -eu

# Official schema bootstrap for the pinned server image; safe to rerun with
# the same version. Database creation belongs to PostgreSQL's first-run SQL.
# https://github.com/temporalio/samples-server/blob/main/compose/scripts/setup-postgres.sh
for database in temporal temporal_visibility; do
  schema=temporal
  if [ "$database" = temporal_visibility ]; then
    schema=visibility
  fi
  temporal-sql-tool --plugin postgres12 --ep postgres --port 5432 \
    --user temporal --db "$database" setup-schema --version 0.0
  temporal-sql-tool --plugin postgres12 --ep postgres --port 5432 \
    --user temporal --db "$database" update-schema \
    --schema-dir "/etc/temporal/schema/postgresql/v12/$schema/versioned"
done
