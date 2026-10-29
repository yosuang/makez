-- name: ClaimAuthorization :many
WITH claimed AS (
 SELECT id FROM makez_authz_outbox WHERE published_at IS NULL AND dead_at IS NULL AND available_at<=now()
 AND (lease_until IS NULL OR lease_until<=now()) ORDER BY available_at,id LIMIT 20 FOR UPDATE SKIP LOCKED
)
UPDATE makez_authz_outbox o SET lease_owner=$1,lease_token=o.lease_token+1,lease_until=now()+interval '30 seconds'
FROM claimed WHERE o.id=claimed.id RETURNING o.*;
-- name: AckAuthorization :execrows
UPDATE makez_authz_outbox SET published_at=now(),lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND published_at IS NULL;
-- name: RetryAuthorization :execrows
UPDATE makez_authz_outbox SET attempts=attempts+1,available_at=now()+make_interval(secs=>LEAST(300,power(2,LEAST(attempts,8)))*(0.75+random()*0.5)),
dead_at=CASE WHEN attempts>=11 THEN now() ELSE NULL END,lease_owner=NULL,lease_until=NULL
WHERE id=$1 AND lease_owner=$2 AND lease_token=$3 AND lease_until>now() AND published_at IS NULL;
