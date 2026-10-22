#!/bin/sh
set -eu

# setup-schema creates version tables if absent and preserves newer versions.
# update-schema applies only pending migrations; never use setup's --overwrite.
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
