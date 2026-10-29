-- name: ArtifactAuditPage :many
SELECT * FROM makez_artifacts WHERE id>$1 ORDER BY id LIMIT 100;
-- name: ArtifactKeyExists :one
SELECT EXISTS(SELECT FROM makez_artifacts WHERE storage_key=$1);
