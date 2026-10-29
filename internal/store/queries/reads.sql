-- name: GetTask :one
SELECT t.*,COALESCE((SELECT last_error FROM makez_outbox WHERE task_id=t.id AND command='START_TASK' AND delivered_at IS NULL),'')::text AS dispatch_error FROM makez_tasks t WHERE t.workspace_id=$1 AND t.id=$2;

-- name: LockTask :one
SELECT t.*,COALESCE((SELECT last_error FROM makez_outbox WHERE task_id=t.id AND command='START_TASK' AND delivered_at IS NULL),'')::text AS dispatch_error FROM makez_tasks t WHERE t.workspace_id=$1 AND t.id=$2 FOR UPDATE;

-- name: ListTasks :many
SELECT t.*,COALESCE((SELECT last_error FROM makez_outbox WHERE task_id=t.id AND command='START_TASK' AND delivered_at IS NULL),'')::text AS dispatch_error FROM makez_tasks t WHERE t.workspace_id=$1 ORDER BY t.created_at DESC,t.id DESC LIMIT 100;

-- name: RecoveryTasks :many
SELECT t.*,COALESCE((SELECT last_error FROM makez_outbox WHERE task_id=t.id AND command='START_TASK' AND delivered_at IS NULL),'')::text AS dispatch_error FROM makez_tasks t WHERE (t.state NOT IN ('SUCCEEDED','FAILED','CANCELED') OR EXISTS(SELECT 1 FROM makez_runs r WHERE r.task_id=t.id AND r.cleanup='PENDING')) AND ($2::text='' OR (t.created_at,t.id)>($1::timestamptz,$2)) AND t.created_at<=$3::timestamptz ORDER BY t.created_at,t.id LIMIT 100;

-- name: GetRunByTask :one
SELECT * FROM makez_runs WHERE task_id=$1;

-- name: LockRun :one
SELECT * FROM makez_runs WHERE id=$1 FOR UPDATE;

-- name: FindIdempotency :one
SELECT id,request_hash FROM makez_tasks WHERE workspace_id=$1 AND idempotency_key=$2 FOR UPDATE;

-- name: LockOperationTask :one
SELECT t.state,t.deadline FROM makez_tasks t JOIN makez_runs r ON r.task_id=t.id WHERE r.id=$1 FOR UPDATE OF t;

-- name: GetOperation :one
SELECT request_hash,phase FROM makez_operations WHERE run_id=$1 AND step=$2;

-- name: FreezeAgentPlan :one
UPDATE makez_runs SET agent_plan=COALESCE(agent_plan,$2::jsonb),version=version+CASE WHEN agent_plan IS NULL THEN 1 ELSE 0 END WHERE id=$1 AND (agent_plan IS NOT NULL OR state IN ('PREPARING','STARTING')) RETURNING agent_plan;

-- name: ClaimOutputObserver :one
UPDATE makez_runs SET observer_epoch=observer_epoch+1 WHERE id=$1 AND state NOT IN ('SUCCEEDED','FAILED','CANCELED') RETURNING observer_epoch;

-- name: GetEvents :one
SELECT COALESCE((SELECT jsonb_agg(page ORDER BY page.seq) FROM (SELECT e.seq,e.run_id,e.type,e.payload,e.created_at FROM makez_events e JOIN makez_runs r ON r.id=e.run_id WHERE r.task_id=t.id AND e.workspace_id=t.workspace_id AND e.seq>$3 ORDER BY e.seq LIMIT 500) page),'[]'::jsonb)::jsonb AS events FROM makez_tasks t WHERE t.workspace_id=$1 AND t.id=$2;

-- name: ListArtifacts :many
SELECT a.* FROM makez_artifacts a JOIN makez_runs r ON r.id=a.run_id AND r.workspace_id=a.workspace_id WHERE a.workspace_id=$1 AND r.task_id=$2 ORDER BY a.path;

-- name: GetArtifact :one
SELECT * FROM makez_artifacts WHERE workspace_id=$1 AND id=$2;

-- name: OutboxBacklog :one
SELECT count(*) AS pending,COALESCE(EXTRACT(EPOCH FROM now()-min(created_at)),0)::float8 AS oldest_seconds FROM makez_outbox WHERE delivered_at IS NULL;

-- name: CleanupBacklog :one
SELECT count(*) FILTER(WHERE cleanup='PENDING') AS pending,count(*) FILTER(WHERE cleanup='BLOCKED') AS blocked FROM makez_runs WHERE cleanup IN ('PENDING','BLOCKED');
