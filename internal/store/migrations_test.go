package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

func legacyFixture(t *testing.T, version int) *Store {
	t.Helper()
	s, _ := testStore(t)
	// Reconstruct only this test's isolated schema, never a shared database.
	var privateSchema string
	if err := s.pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&privateSchema); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(privateSchema, "makez_store_test_") {
		t.Fatal("refusing non-test schema")
	}
	_, err := s.pool.Exec(t.Context(), `DROP SCHEMA `+privateSchema+` CASCADE; CREATE SCHEMA `+privateSchema+`; CREATE TABLE makez_schema_migrations(version integer PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now())`)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{"00001_legacy.sql", "00002_agent_plan.sql", "00003_operations.sql"}
	for i, name := range files[:version] {
		data, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(t.Context(), string(data)); err != nil {
			t.Fatal(err)
		}
		if _, err = s.pool.Exec(t.Context(), `INSERT INTO makez_schema_migrations(version) VALUES($1)`, i+1); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.pool.Exec(t.Context(), `INSERT INTO makez_tasks(id,workspace_id,idempotency_key,request_hash,prompt,agent_id,agent_version,profile_id,profile_version,state,deadline) VALUES('old-task','legacy-text-workspace','key','hash','preserve me','a','1','p','1','QUEUED',now()+interval '1 day');
INSERT INTO makez_runs(id,task_id,state,backend_id) VALUES('old-run','old-task','PREPARING','backend');
INSERT INTO makez_outbox(id,workspace_id,task_id,command) VALUES('old-event','legacy-text-workspace','old-task','START_TASK')`)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLegacyAdoptionPreservesData(t *testing.T) {
	for version := 1; version <= 3; version++ {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			s := legacyFixture(t, version)
			if err := s.Migrate(t.Context()); err == nil {
				t.Fatal("implicit adoption accepted")
			}
			for range 2 {
				if err := s.MigrateLegacy(t.Context(), true); err != nil {
					t.Fatal(err)
				}
			}
			task, err := s.Get(t.Context(), "legacy-text-workspace", "old-task")
			if err != nil || task.Prompt != "preserve me" || task.Run == nil || task.Run.ID != "old-run" || task.Run.StartedAt != nil {
				t.Fatalf("data changed: %+v %v", task, err)
			}
			var count int
			if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_schema_migrations`).Scan(&count); err != nil || count != version {
				t.Fatalf("legacy ledger changed: %d %v", count, err)
			}
			if err = s.CheckSchema(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLegacyAdoptionRejectsDrift(t *testing.T) {
	s := legacyFixture(t, 2)
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE makez_tasks ALTER COLUMN prompt DROP NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(t.Context(), true); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("drift not rejected: %v", err)
	}
	var exists bool
	if err := s.pool.QueryRow(t.Context(), `SELECT to_regclass('makez_goose_version') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("failed adoption persisted ledger: %v %v", exists, err)
	}
}

func TestMigrationConcurrency(t *testing.T) {
	s := legacyFixture(t, 1)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if err := s.MigrateLegacy(t.Context(), true); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := s.CheckSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationFailurePreservesRowsAndCanResume(t *testing.T) {
	s := legacyFixture(t, 3)
	if _, err := s.pool.Exec(t.Context(), `UPDATE makez_outbox SET workspace_id='unproven-workspace'`); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(t.Context(), true); err == nil {
		t.Fatal("invalid cross-workspace data silently migrated")
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_outbox WHERE workspace_id='unproven-workspace'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed migration destroyed evidence", err)
	}
	// An operator resolves the fixture's deliberately injected corruption using the task record.
	if _, err := s.pool.Exec(t.Context(), `UPDATE makez_outbox o SET workspace_id=t.workspace_id FROM makez_tasks t WHERE t.id=o.task_id`); err != nil {
		t.Fatal(err)
	}
	if err := s.MigrateLegacy(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestApplicationRoleRequiresNoDDL(t *testing.T) {
	s, _ := testStore(t)
	role := "makez_app_test_" + newID()
	var schema string
	if err := s.pool.QueryRow(t.Context(), "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `CREATE ROLE `+role+` NOLOGIN; GRANT USAGE ON SCHEMA `+schema+` TO `+role+`; GRANT SELECT,INSERT,UPDATE ON ALL TABLES IN SCHEMA `+schema+` TO `+role+`; GRANT USAGE ON ALL SEQUENCES IN SCHEMA `+schema+` TO `+role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := s.pool.Exec(context.Background(), `DROP OWNED BY `+role+`; DROP ROLE `+role); err != nil {
			t.Error(err)
		}
	})
	config := s.pool.Config().ConnConfig.Copy()
	config.RuntimeParams["role"] = role
	conn, err := pgx.ConnectConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck // Read-only fixture connection.
	var version int
	if err = conn.QueryRow(t.Context(), `SELECT max(version_id) FROM makez_goose_version WHERE is_applied`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("application role cannot check version", err)
	}
	if _, err = conn.Exec(t.Context(), `CREATE TABLE forbidden_ddl(id integer)`); err == nil {
		t.Fatal("application role has DDL permission")
	}
}
