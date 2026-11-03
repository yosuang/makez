-- name: AuditOutboxReplay :exec
INSERT INTO makez_audit_events(id,workspace_id,actor_kind,actor_id,action,outcome,resource_id,detail)
VALUES($1,$2,'operator','maintenance-command','replay_outbox','allowed',$3,
 jsonb_build_object('queue',sqlc.arg(queue)::text,'reason',sqlc.arg(reason)::text));

-- name: OutboxQueueCounts :many
SELECT 'task'::text AS queue,
 count(*) FILTER (WHERE dead_at IS NULL) AS pending,
 count(*) FILTER (WHERE dead_at IS NOT NULL) AS dead
FROM makez_outbox o WHERE o.workspace_id=$1 AND o.delivered_at IS NULL
UNION ALL
SELECT 'authorization'::text AS queue,
 count(*) FILTER (WHERE dead_at IS NULL) AS pending,
 count(*) FILTER (WHERE dead_at IS NOT NULL) AS dead
FROM makez_authz_outbox a WHERE a.workspace_id=$1 AND a.published_at IS NULL;

-- name: ListDeadLetters :many
SELECT 'task'::text AS queue,id,attempts::bigint AS attempts,dead_at,
 'delivery attempts exhausted; outcome may be unknown; operator replay required'::text AS reason
FROM makez_outbox o WHERE o.workspace_id=$1 AND o.delivered_at IS NULL AND o.dead_at IS NOT NULL
UNION ALL
SELECT 'authorization'::text AS queue,id::text,attempts,dead_at,
 'delivery attempts exhausted; notification may have been observed; operator replay required'::text AS reason
FROM makez_authz_outbox a WHERE a.workspace_id=$1 AND a.published_at IS NULL AND a.dead_at IS NOT NULL
ORDER BY dead_at,queue,id LIMIT 101;
