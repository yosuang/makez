-- name: LockUser :one
SELECT * FROM makez_users WHERE id=$1 FOR UPDATE;
-- name: InsertSession :exec
INSERT INTO makez_sessions(id,user_id,auth_epoch,created_at,expires_at,idle_expires_at) VALUES($1,$2,$3,$4,$5,$6);
-- name: InsertRefresh :exec
INSERT INTO makez_refresh_tokens(id,session_id,token_digest,generation,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6);
-- name: InsertAccess :exec
INSERT INTO makez_access_tokens(id,session_id,token_digest,created_at,expires_at) VALUES($1,$2,$3,$4,$5);
-- name: AuthenticateAccess :one
SELECT u.id FROM makez_access_tokens a JOIN makez_sessions s ON s.id=a.session_id JOIN makez_users u ON u.id=s.user_id
WHERE a.token_digest=$1 AND a.revoked_at IS NULL AND a.expires_at>$2 AND s.revoked_at IS NULL
AND s.expires_at>$2 AND s.idle_expires_at>$2 AND u.status='active' AND u.auth_epoch=s.auth_epoch;
-- name: FindRefreshSession :one
SELECT s.id,s.user_id FROM makez_refresh_tokens r JOIN makez_sessions s ON s.id=r.session_id WHERE r.token_digest=$1;
-- name: LockSession :one
SELECT * FROM makez_sessions WHERE id=$1 FOR UPDATE;
-- name: LockRefresh :one
SELECT * FROM makez_refresh_tokens WHERE token_digest=$1 FOR UPDATE;
-- name: ConsumeRefresh :exec
UPDATE makez_refresh_tokens SET used_at=$2 WHERE id=$1;
-- name: TouchSession :exec
UPDATE makez_sessions SET last_seen_at=$2,idle_expires_at=$3 WHERE id=$1;
-- name: RevokeSession :exec
UPDATE makez_sessions SET revoked_at=COALESCE(revoked_at,$2) WHERE id=$1;
-- name: RevokeAllSessions :exec
UPDATE makez_sessions SET revoked_at=COALESCE(revoked_at,$2) WHERE user_id=$1;
-- name: BumpUserEpoch :exec
UPDATE makez_users SET auth_epoch=auth_epoch+1,updated_at=now() WHERE id=$1;
-- name: CreateLoginTransaction :exec
INSERT INTO makez_login_transactions(state_digest,browser_digest,nonce,verifier,expires_at) VALUES($1,$2,$3,$4,$5);
-- name: ConsumeLoginTransaction :one
UPDATE makez_login_transactions SET used_at=now() WHERE state_digest=$1 AND browser_digest=$2 AND used_at IS NULL AND expires_at>now() RETURNING nonce,verifier;
