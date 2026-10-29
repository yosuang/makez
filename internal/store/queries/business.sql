-- name: EnqueueCommand :execrows
INSERT INTO makez_outbox(id,workspace_id,task_id,run_id,command) VALUES($1,$2,$3,$4,$5) ON CONFLICT(task_id,command) DO NOTHING;

-- name: InsertTask :execrows
INSERT INTO makez_tasks(id,workspace_id,idempotency_key,request_hash,prompt,config,agent_id,agent_version,profile_id,profile_version,state,deadline) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'QUEUED',$11) ON CONFLICT(workspace_id,idempotency_key) DO NOTHING;

-- name: CancelTaskState :execrows
UPDATE makez_tasks SET state=$2,reason=$3,cancel_requested=true,version=version+1 WHERE id=$1;

-- name: ExpireTaskState :execrows
UPDATE makez_tasks SET state=$2,reason=$3,version=version+1 WHERE id=$1;

-- name: InitializeBusinessRun :execrows
INSERT INTO makez_runs(id,task_id,state,backend_id,started_at,workspace_id) VALUES($1,$2,$3,$4,$5,(SELECT workspace_id FROM makez_tasks WHERE id=$2));

-- name: MarkTaskRunning :execrows
UPDATE makez_tasks SET state='RUNNING',version=version+1 WHERE id=$1;

-- name: UpdateExecution :execrows
UPDATE makez_runs SET state=$2,sandbox=$3,process=$4,cursor=$5,parser_state=$6,process_exit=$7,cleanup=$8,verified_stopped=$9,prepared_result=$10,version=version+1 WHERE id=$1;

-- name: InsertArtifact :execrows
INSERT INTO makez_artifacts(id,run_id,path,storage_key,sha256,media_type,size,workspace_id) VALUES($1,$2,$3,$4,$5,$6,$7,(SELECT workspace_id FROM makez_runs WHERE id=$2));

-- name: PublishExecution :execrows
UPDATE makez_runs SET state=$2,verified_stopped=$3,cleanup=$4,version=version+1 WHERE id=$1;

-- name: PublishTaskState :execrows
UPDATE makez_tasks SET state=$2,reason=$3,version=version+1 WHERE id=$1;

-- name: SetCleanupState :execrows
UPDATE makez_runs SET cleanup=$2,version=version+1 WHERE id=$1 AND (cleanup!='DONE' OR $2='DONE');

-- name: InsertOperation :execrows
INSERT INTO makez_operations(run_id,step,request_hash,phase,workspace_id) VALUES($1,$2,$3,'DISPATCHING',(SELECT workspace_id FROM makez_runs WHERE id=$1));

-- name: ConfirmOperationState :execrows
UPDATE makez_operations SET phase='CONFIRMED',updated_at=now() WHERE run_id=$1 AND step=$2;

-- name: InsertOutput :execrows
INSERT INTO makez_events(run_id,source_offset,item_index,type,payload,workspace_id) VALUES($1,$2,$3,$4,$5,(SELECT workspace_id FROM makez_runs WHERE id=$1)) ON CONFLICT(run_id,source_offset,item_index) DO NOTHING;

-- name: SaveOutputCursor :execrows
UPDATE makez_runs SET cursor=$2,parser_state=$3,version=version+1 WHERE id=$1;
