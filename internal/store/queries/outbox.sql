-- Attempts count committed delivery leases since the last operator replay.
-- Upgrade dispatchers together: old versions count explicit retries instead.
-- Historical attempts remain a lower bound; lease_token spans replay cycles.

-- name: RecoverExpiredOutbox :execrows
WITH expired AS (
 SELECT id FROM makez_outbox
 WHERE delivered_at IS NULL AND dead_at IS NULL
 AND (lease_until<=now() OR (lease_until IS NULL AND attempts>=12))
 ORDER BY next_attempt_at,id LIMIT 100 FOR UPDATE SKIP LOCKED
)
UPDATE makez_outbox o SET
 last_error=CASE WHEN o.attempts>=12 THEN 'delivery dead-lettered; operator replay required' ELSE 'delivery lease expired; outcome may be unknown' END,
 next_attempt_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(GREATEST(o.attempts-1,0),8))*(0.75+random()*0.5))),
 dead_at=CASE WHEN o.attempts>=12 THEN now() ELSE NULL END,
 lease_owner=NULL,lease_until=NULL
FROM expired WHERE o.id=expired.id;

-- name: ClaimOutbox :many
WITH claimed AS (
 SELECT id FROM makez_outbox
 WHERE delivered_at IS NULL AND dead_at IS NULL AND next_attempt_at<=now()
 AND lease_until IS NULL AND attempts<12
 ORDER BY next_attempt_at,id LIMIT $1 FOR UPDATE SKIP LOCKED
)
UPDATE makez_outbox o SET attempts=o.attempts+1,lease_owner=$2,lease_token=o.lease_token+1,
lease_until=now()+interval '30 seconds'
FROM claimed WHERE o.id=claimed.id RETURNING o.*;

-- name: AckOutbox :execrows
UPDATE makez_outbox SET delivered_at=now(),last_error='',lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND delivered_at IS NULL AND dead_at IS NULL;

-- name: RetryClaimedOutbox :execrows
UPDATE makez_outbox SET last_error=CASE WHEN attempts>=12 THEN 'delivery dead-lettered; operator replay required' ELSE $4 END,
next_attempt_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(GREATEST(attempts-1,0),8))*(0.75+random()*0.5))),
dead_at=CASE WHEN attempts>=12 THEN now() ELSE NULL END,lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND delivered_at IS NULL AND dead_at IS NULL;

-- name: ReplayDeadOutbox :execrows
UPDATE makez_outbox SET dead_at=NULL,attempts=0,next_attempt_at=now(),last_error='',
lease_owner=NULL,lease_until=NULL,lease_token=lease_token+1
WHERE id=$1 AND workspace_id=$2 AND dead_at IS NOT NULL AND delivered_at IS NULL;
