-- Keep delivery budgets, lease duration, and backoff aligned with outbox.sql.
-- Attempts count committed leases, including deliveries interrupted by a crash.

-- name: RecoverExpiredAuthorization :execrows
WITH expired AS (
 SELECT id FROM makez_authz_outbox
 WHERE published_at IS NULL AND dead_at IS NULL
 AND (lease_until<=now() OR (lease_until IS NULL AND attempts>=12))
 ORDER BY available_at,id LIMIT 100 FOR UPDATE SKIP LOCKED
)
UPDATE makez_authz_outbox o SET
 available_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(GREATEST(o.attempts-1,0),8))*(0.75+random()*0.5))),
 dead_at=CASE WHEN o.attempts>=12 THEN now() ELSE NULL END,
 lease_owner=NULL,lease_until=NULL
FROM expired WHERE o.id=expired.id;

-- name: ClaimAuthorization :many
WITH claimed AS (
 SELECT id FROM makez_authz_outbox WHERE published_at IS NULL AND dead_at IS NULL AND available_at<=now()
 AND lease_until IS NULL AND attempts<12 ORDER BY available_at,id LIMIT 20 FOR UPDATE SKIP LOCKED
)
UPDATE makez_authz_outbox o SET attempts=o.attempts+1,lease_owner=$1,lease_token=o.lease_token+1,lease_until=now()+interval '30 seconds'
FROM claimed WHERE o.id=claimed.id RETURNING o.*;

-- name: AckAuthorization :execrows
UPDATE makez_authz_outbox SET published_at=now(),lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND published_at IS NULL AND dead_at IS NULL;

-- name: RetryAuthorization :execrows
UPDATE makez_authz_outbox SET available_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(GREATEST(attempts-1,0),8))*(0.75+random()*0.5))),
dead_at=CASE WHEN attempts>=12 THEN now() ELSE NULL END,lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND published_at IS NULL AND dead_at IS NULL;

-- name: ReplayDeadAuthorization :execrows
UPDATE makez_authz_outbox SET dead_at=NULL,attempts=0,available_at=now(),
lease_owner=NULL,lease_until=NULL,lease_token=lease_token+1
WHERE id=$1 AND workspace_id=$2 AND dead_at IS NOT NULL AND published_at IS NULL;
