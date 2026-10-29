-- name: FindIdentity :one
SELECT u.* FROM makez_users u JOIN makez_external_identities i ON i.user_id=u.id WHERE i.issuer=$1 AND i.subject=$2;
-- name: CreateUser :exec
INSERT INTO makez_users(id,display_name) VALUES($1,$2);
-- name: CreateIdentity :exec
INSERT INTO makez_external_identities(id,user_id,issuer,subject,email,email_verified) VALUES($1,$2,$3,$4,$5,$6);
-- name: UpdateIdentity :exec
UPDATE makez_external_identities SET email=$3,email_verified=$4,last_login_at=now() WHERE issuer=$1 AND subject=$2;
-- name: CreateWorkspace :exec
INSERT INTO makez_workspaces(id,slug,bootstrap_user_id) VALUES($1,$2,$3);
-- name: CreateMembership :exec
INSERT INTO makez_memberships(workspace_id,user_id,role) VALUES($1,$2,$3);
-- name: Audit :exec
INSERT INTO makez_audit_events(id,workspace_id,actor_kind,actor_id,action,outcome,resource_id,request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8);
-- name: GetUser :one
SELECT * FROM makez_users WHERE id=$1;
-- name: ListWorkspaces :many
SELECT w.id,w.slug,m.role,w.authz_version FROM makez_workspaces w JOIN makez_memberships m ON m.workspace_id=w.id
JOIN makez_users u ON u.id=m.user_id
WHERE m.user_id=$1 AND m.status='active' AND w.status='active' AND u.status='active' ORDER BY w.id;
-- name: WorkspaceShareLock :one
SELECT status FROM makez_workspaces WHERE id=$1 FOR SHARE;
-- name: WorkspaceExclusiveLock :one
SELECT status FROM makez_workspaces WHERE id=$1 FOR UPDATE;
-- name: UserRole :one
SELECT m.role FROM makez_memberships m JOIN makez_users u ON u.id=m.user_id
WHERE m.workspace_id=$1 AND m.user_id=$2 AND m.status='active' AND u.status='active';
-- name: MachineRole :one
SELECT role FROM makez_machines WHERE workspace_id=$1 AND id=$2 AND status='active';
-- name: FindMachine :one
SELECT id,workspace_id,role FROM makez_machines WHERE credential_digest=$1 AND status='active';
-- name: ListMembers :many
SELECT m.user_id,m.role,u.display_name FROM makez_memberships m JOIN makez_users u ON u.id=m.user_id WHERE m.workspace_id=$1 AND m.status='active' ORDER BY m.user_id;
-- name: CountAdmins :one
SELECT count(*) FROM makez_memberships WHERE workspace_id=$1 AND role='Admin' AND status='active';
-- name: UpdateMember :execrows
UPDATE makez_memberships SET role=$3,status=$4,updated_at=now() WHERE workspace_id=$1 AND user_id=$2 AND status='active';
-- name: BumpAuthorization :one
UPDATE makez_workspaces SET authz_version=authz_version+1,updated_at=now() WHERE id=$1 RETURNING authz_version;
-- name: EnqueueAuthorization :exec
INSERT INTO makez_authz_outbox(id,workspace_id,authz_version,event_type) VALUES($1,$2,$3,'membership_changed');
-- name: SaveTaskActor :exec
UPDATE makez_tasks SET submitted_by_user_id=$3,submitted_by_machine_id=$4 WHERE workspace_id=$1 AND id=$2;
-- name: GetTaskActor :one
SELECT submitted_by_user_id,submitted_by_machine_id FROM makez_tasks WHERE workspace_id=$1 AND id=$2;
-- name: RegisterMachineWorkspace :exec
INSERT INTO makez_workspaces(id,slug) VALUES($1,$2) ON CONFLICT(id) DO NOTHING;
-- name: RegisterMachine :exec
INSERT INTO makez_machines(id,workspace_id,credential_digest) VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING;
-- name: MachineWorkspace :one
SELECT workspace_id FROM makez_machines WHERE id=$1;
-- name: RevokeMachine :execrows
UPDATE makez_machines SET status='revoked' WHERE workspace_id=$1 AND id=$2;
-- name: FindBootstrap :one
SELECT id FROM makez_workspaces WHERE bootstrap_user_id=$1;
