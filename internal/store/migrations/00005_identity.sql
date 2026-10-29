-- +goose Up
CREATE TABLE makez_users (
 id uuid PRIMARY KEY,
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','disabled')),
 auth_epoch bigint NOT NULL DEFAULT 0 CHECK(auth_epoch>=0),
 display_name text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE makez_external_identities (
 id uuid PRIMARY KEY,user_id uuid NOT NULL REFERENCES makez_users(id),
 issuer text NOT NULL,subject text NOT NULL,email text NOT NULL DEFAULT '',email_verified boolean NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),last_login_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(issuer,subject)
);
CREATE INDEX makez_identity_user ON makez_external_identities(user_id);
CREATE TABLE makez_workspaces (
 id text PRIMARY KEY CHECK(length(id) BETWEEN 1 AND 128),
 bootstrap_user_id uuid UNIQUE REFERENCES makez_users(id),slug text NOT NULL UNIQUE,
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','suspended','deleting')),
 authz_version bigint NOT NULL DEFAULT 0 CHECK(authz_version>=0),
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now()
);
-- Existing identifiers are preserved. This creates no user memberships or machine grants.
INSERT INTO makez_workspaces(id,slug) SELECT DISTINCT workspace_id,'legacy-'||encode(sha256(convert_to(workspace_id,'UTF8')),'hex') FROM makez_tasks;
ALTER TABLE makez_tasks ADD CONSTRAINT makez_tasks_workspace_fk FOREIGN KEY(workspace_id) REFERENCES makez_workspaces(id);
CREATE TABLE makez_memberships (
 workspace_id text NOT NULL REFERENCES makez_workspaces(id),user_id uuid NOT NULL REFERENCES makez_users(id),
 role text NOT NULL CHECK(role IN ('Admin','Operator','Viewer')),
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','revoked')),
 created_at timestamptz NOT NULL DEFAULT now(),updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(workspace_id,user_id)
);
CREATE INDEX makez_membership_user ON makez_memberships(user_id,status,workspace_id);
CREATE TABLE makez_machines (
 id text PRIMARY KEY,workspace_id text NOT NULL REFERENCES makez_workspaces(id),
 credential_digest bytea NOT NULL UNIQUE CHECK(octet_length(credential_digest)=32),
 status text NOT NULL DEFAULT 'active' CHECK(status IN ('active','revoked')),
 role text NOT NULL DEFAULT 'Operator' CHECK(role IN ('Operator','Viewer')),
 created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE makez_tasks ADD COLUMN submitted_by_user_id uuid REFERENCES makez_users(id);
ALTER TABLE makez_tasks ADD COLUMN submitted_by_machine_id text REFERENCES makez_machines(id);
ALTER TABLE makez_tasks ADD CONSTRAINT makez_tasks_one_actor CHECK(NOT(submitted_by_user_id IS NOT NULL AND submitted_by_machine_id IS NOT NULL));
CREATE TABLE makez_audit_events (
 id uuid PRIMARY KEY,workspace_id text REFERENCES makez_workspaces(id),
 actor_kind text NOT NULL,actor_id text NOT NULL DEFAULT '',action text NOT NULL,outcome text NOT NULL,
 resource_id text NOT NULL DEFAULT '',request_id text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT now(),detail jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX makez_audit_workspace ON makez_audit_events(workspace_id,created_at,id);
CREATE TABLE makez_authz_outbox (
 id uuid PRIMARY KEY,workspace_id text NOT NULL REFERENCES makez_workspaces(id),authz_version bigint NOT NULL CHECK(authz_version>=0),
 event_type text NOT NULL,payload jsonb NOT NULL DEFAULT '{}',attempts bigint NOT NULL DEFAULT 0 CHECK(attempts>=0),
 lease_token bigint NOT NULL DEFAULT 0 CHECK(lease_token>=0),lease_owner text,lease_until timestamptz,
 available_at timestamptz NOT NULL DEFAULT now(),created_at timestamptz NOT NULL DEFAULT now(),published_at timestamptz,dead_at timestamptz,
 UNIQUE(workspace_id,authz_version),CHECK((lease_owner IS NULL)=(lease_until IS NULL))
);
CREATE INDEX makez_authz_outbox_pending ON makez_authz_outbox(available_at,id) WHERE published_at IS NULL AND dead_at IS NULL;
CREATE TABLE makez_sessions (
 id uuid PRIMARY KEY,user_id uuid NOT NULL REFERENCES makez_users(id),auth_epoch bigint NOT NULL CHECK(auth_epoch>=0),
 created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,idle_expires_at timestamptz NOT NULL,
 revoked_at timestamptz,last_seen_at timestamptz,client_kind text NOT NULL DEFAULT 'browser',
 CHECK(expires_at>created_at AND idle_expires_at>created_at AND idle_expires_at<=expires_at)
);
CREATE INDEX makez_session_user ON makez_sessions(user_id,created_at);
CREATE TABLE makez_refresh_tokens (
 id uuid PRIMARY KEY,session_id uuid NOT NULL REFERENCES makez_sessions(id),
 token_digest bytea NOT NULL UNIQUE CHECK(octet_length(token_digest)=32),generation bigint NOT NULL CHECK(generation>=0),
 created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,used_at timestamptz,revoked_at timestamptz,
 UNIQUE(session_id,generation),CHECK(expires_at>created_at)
);
CREATE TABLE makez_access_tokens (
 id uuid PRIMARY KEY,session_id uuid NOT NULL REFERENCES makez_sessions(id),
 token_digest bytea NOT NULL UNIQUE CHECK(octet_length(token_digest)=32),
 created_at timestamptz NOT NULL,expires_at timestamptz NOT NULL,revoked_at timestamptz,
 CHECK(expires_at>created_at)
);
CREATE TABLE makez_login_transactions (
 state_digest bytea PRIMARY KEY CHECK(octet_length(state_digest)=32),
 browser_digest bytea NOT NULL CHECK(octet_length(browser_digest)=32),nonce text NOT NULL,verifier text NOT NULL,
 expires_at timestamptz NOT NULL,used_at timestamptz
);
