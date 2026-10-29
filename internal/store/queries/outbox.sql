-- name: ClaimOutbox :many
WITH claimed AS (
 SELECT id FROM makez_outbox
 WHERE delivered_at IS NULL AND dead_at IS NULL AND next_attempt_at<=now()
 AND (lease_until IS NULL OR lease_until<=now())
 ORDER BY next_attempt_at,id LIMIT $1 FOR UPDATE SKIP LOCKED
)
UPDATE makez_outbox o SET lease_owner=$2,lease_token=o.lease_token+1,
lease_until=now()+interval '30 seconds'
FROM claimed WHERE o.id=claimed.id RETURNING o.*;

-- name: AckOutbox :execrows
UPDATE makez_outbox SET delivered_at=now(),last_error='',lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND delivered_at IS NULL;

-- name: RetryClaimedOutbox :execrows
UPDATE makez_outbox SET attempts=attempts+1,last_error=CASE WHEN attempts>=11 THEN 'delivery dead-lettered; operator replay required' ELSE $4 END,
next_attempt_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(attempts,8)))*(0.75+random()*0.5)),
dead_at=CASE WHEN attempts>=11 THEN now() ELSE NULL END,lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND delivered_at IS NULL;

-- name: ReplayDeadOutbox :execrows
UPDATE makez_outbox SET dead_at=NULL,attempts=0,next_attempt_at=now(),last_error=''
WHERE id=$1 AND workspace_id=$2 AND dead_at IS NOT NULL AND delivered_at IS NULL;
