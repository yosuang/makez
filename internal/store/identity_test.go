package store

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/makez-dev/makez/internal/store/dbgen"
)

func loginUser(t *testing.T, s *Store, subject string) (Principal, string) {
	t.Helper()
	u, err := s.LoginIdentity(t.Context(), Identity{Issuer: "https://accounts.google.com", Subject: subject, Email: "same@example.com", EmailVerified: true, Name: subject})
	if err != nil {
		t.Fatal(err)
	}
	p := Principal{Kind: "user", ID: u.ID}
	workspaces, err := s.Workspaces(t.Context(), p)
	if err != nil || len(workspaces) != 1 {
		t.Fatalf("bootstrap workspaces: %v %v", workspaces, err)
	}
	return p, workspaces[0].ID
}

func TestConcurrentIdentityBootstrap(t *testing.T) {
	s, _ := testStore(t)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for range 20 {
		wg.Go(func() {
			u, err := s.LoginIdentity(t.Context(), Identity{Issuer: "https://accounts.google.com", Subject: "same-subject", Email: "same@example.com", EmailVerified: true})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- u.ID
		})
	}
	wg.Wait()
	close(ids)
	id := ""
	for value := range ids {
		if id != "" && value != id {
			t.Fatal("duplicate identity")
		}
		id = value
	}
	for _, query := range []string{`SELECT count(*) FROM makez_users`, `SELECT count(*) FROM makez_external_identities`, `SELECT count(*) FROM makez_workspaces WHERE bootstrap_user_id IS NOT NULL`, `SELECT count(*) FROM makez_memberships`} {
		var count int
		if err := s.pool.QueryRow(t.Context(), query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("bootstrap count %d: %v", count, err)
		}
	}
	other, _ := loginUser(t, s, "different-subject")
	if other.ID == id {
		t.Fatal("email merged distinct subjects")
	}
}

func TestBootstrapFailureRollsBack(t *testing.T) {
	s, _ := testStore(t)
	_, err := s.pool.Exec(t.Context(), `CREATE FUNCTION reject_registration() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected audit failure'; END $$;
CREATE TRIGGER fail_registration BEFORE INSERT ON makez_audit_events FOR EACH ROW EXECUTE FUNCTION reject_registration()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.LoginIdentity(t.Context(), Identity{Issuer: "https://accounts.google.com", Subject: "rollback"}); err == nil {
		t.Fatal("injected failure succeeded")
	}
	var count int
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_users`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial registration: %d %v", count, err)
	}
}

func TestSessionRotationReplayAndRevocation(t *testing.T) {
	s, _ := testStore(t)
	p, _ := loginUser(t, s, "rotation")
	policy := DefaultSessionPolicy()
	first, err := s.CreateSession(t.Context(), p.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.AuthenticateAccess(t.Context(), first.Access); err != nil || got.ID != p.ID {
		t.Fatal("access rejected", err)
	}
	second, err := s.Refresh(t.Context(), first.Refresh, policy)
	if err != nil || second.Refresh == first.Refresh || second.Access == first.Access {
		t.Fatal("rotation failed", err)
	}
	if _, err = s.Refresh(t.Context(), first.Refresh, policy); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("replay accepted", err)
	}
	for _, token := range []string{first.Access, second.Access} {
		if _, err = s.AuthenticateAccess(t.Context(), token); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("family survived replay", err)
		}
	}
	if _, err = s.Refresh(t.Context(), second.Refresh, policy); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("revoked family refreshed", err)
	}
}

func TestConcurrentRefreshRevokesWinnerOnReplay(t *testing.T) {
	s, _ := testStore(t)
	p, _ := loginUser(t, s, "concurrent-refresh")
	policy := DefaultSessionPolicy()
	pair, err := s.CreateSession(t.Context(), p.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	var success atomic.Int32
	var wg sync.WaitGroup
	winners := make(chan TokenPair, 2)
	for range 2 {
		wg.Go(func() {
			got, err := s.Refresh(t.Context(), pair.Refresh, policy)
			if err == nil {
				success.Add(1)
				winners <- got
			} else if !errors.Is(err, ErrUnauthenticated) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	close(winners)
	if success.Load() != 1 {
		t.Fatalf("refresh winners=%d", success.Load())
	}
	for winner := range winners {
		if _, err = s.AuthenticateAccess(t.Context(), winner.Access); !errors.Is(err, ErrUnauthenticated) {
			t.Fatal("winner survived concurrent replay", err)
		}
	}
}

func TestSessionCurrentStateEnforcement(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE makez_access_tokens SET expires_at=created_at+interval '1 microsecond'`,
		`UPDATE makez_sessions SET idle_expires_at=created_at+interval '1 microsecond'`,
		`UPDATE makez_sessions SET expires_at=created_at+interval '1 microsecond',idle_expires_at=created_at+interval '1 microsecond'`,
		`UPDATE makez_users SET status='disabled'`,
		`UPDATE makez_users SET auth_epoch=auth_epoch+1`,
	} {
		t.Run(mutation, func(t *testing.T) {
			s, _ := testStore(t)
			p, _ := loginUser(t, s, "expiration")
			pair, err := s.CreateSession(t.Context(), p.ID, DefaultSessionPolicy())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.pool.Exec(t.Context(), mutation); err != nil {
				t.Fatal(err)
			}
			if _, err = s.AuthenticateAccess(t.Context(), pair.Access); !errors.Is(err, ErrUnauthenticated) {
				t.Fatal("invalid current state accepted", err)
			}
		})
	}
}

func TestLogoutDoesNotRevokeTaskPrincipal(t *testing.T) {
	s, _ := testStore(t)
	p, ws := loginUser(t, s, "logout")
	ctx := WithPrincipal(t.Context(), p)
	task, _, err := s.Submit(ctx, ws, "logout-task", input())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := s.CreateSession(ctx, p.ID, DefaultSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Logout(ctx, pair.Refresh); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateAccess(ctx, pair.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("logout access accepted", err)
	}
	if err = s.AdmitTaskAction(ctx, ws, task.ID, "launch"); err != nil {
		t.Fatal("logout canceled task authorization", err)
	}
	pair, err = s.CreateSession(ctx, p.ID, DefaultSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.LogoutAll(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateAccess(ctx, pair.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("logout-all access accepted", err)
	}
	if err = s.AdmitTaskAction(ctx, ws, task.ID, "launch"); err != nil {
		t.Fatal(err)
	}
}

func TestRoleMatrixAndRevokedTask(t *testing.T) {
	s, _ := testStore(t)
	admin, ws := loginUser(t, s, "admin")
	operator, foreign := loginUser(t, s, "operator")
	q := dbgen.New(s.pool)
	if err := q.CreateMembership(t.Context(), dbgen.CreateMembershipParams{WorkspaceID: ws, UserID: operator.ID, Role: "Operator"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(t.Context(), admin, foreign, "read"); !errors.Is(err, ErrForbidden) {
		t.Fatal("cross-workspace authorized", err)
	}
	workspaces, err := s.Workspaces(t.Context(), admin)
	if err != nil || len(workspaces) != 1 {
		t.Fatal("workspace leak", err)
	}
	task, _, err := s.Submit(WithPrincipal(t.Context(), operator), ws, "revoked-task", input())
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"Admin", "Operator", "Viewer"} {
		if err = s.ChangeMember(t.Context(), admin, ws, operator.ID, role, false); err != nil {
			t.Fatal(err)
		}
		for _, action := range []string{"read", "write", "members"} {
			allowed := action == "read" || (action == "write" && role != "Viewer") || (action == "members" && role == "Admin")
			err = s.Authorize(t.Context(), operator, ws, action)
			if allowed && err != nil || !allowed && !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s %s: %v", role, action, err)
			}
		}
	}
	if err = s.AdmitTaskAction(t.Context(), ws, task.ID, "launch"); !errors.Is(err, ErrForbidden) {
		t.Fatal("demoted actor launched", err)
	}
	if _, _, err = s.Submit(WithPrincipal(t.Context(), operator), ws, "forbidden", input()); !errors.Is(err, ErrForbidden) {
		t.Fatal("viewer wrote transaction", err)
	}
	if err = s.ChangeMember(t.Context(), admin, ws, admin.ID, "Viewer", true); !errors.Is(err, ErrConflict) {
		t.Fatal("last admin removed", err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE makez_workspaces SET status='suspended' WHERE id=$1`, ws); err != nil {
		t.Fatal(err)
	}
	if err = s.Authorize(t.Context(), admin, ws, "read"); !errors.Is(err, ErrForbidden) {
		t.Fatal("suspended workspace visible", err)
	}
}

func TestConcurrentLastAdminChange(t *testing.T) {
	s, _ := testStore(t)
	a, ws := loginUser(t, s, "admin-a")
	b, _ := loginUser(t, s, "admin-b")
	if err := dbgen.New(s.pool).CreateMembership(t.Context(), dbgen.CreateMembershipParams{WorkspaceID: ws, UserID: b.ID, Role: "Admin"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var success atomic.Int32
	for _, p := range []Principal{a, b} {
		wg.Go(func() {
			err := s.ChangeMember(t.Context(), p, ws, p.ID, "Viewer", false)
			if err == nil {
				success.Add(1)
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("admin downgrade winners=%d", success.Load())
	}
}

func TestLegacyActorCannotLaunch(t *testing.T) {
	s := legacyFixture(t, 3)
	if err := s.MigrateLegacy(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := s.AdmitTaskAction(t.Context(), "legacy-text-workspace", "old-task", "launch"); !errors.Is(err, ErrForbidden) {
		t.Fatal("unproven historical actor authorized", err)
	}
}

func TestLoginTransactionReplayAndBinding(t *testing.T) {
	s, _ := testStore(t)
	l, err := s.StartLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeLogin(t.Context(), l.State, opaque("")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("wrong browser accepted", err)
	}
	got, err := s.ConsumeLogin(t.Context(), l.State, l.Browser)
	if err != nil || got.Verifier != l.Verifier || got.Nonce != l.Nonce {
		t.Fatal("correct login rejected", err)
	}
	if _, err = s.ConsumeLogin(t.Context(), l.State, l.Browser); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("callback replay accepted", err)
	}
}

func TestSessionPolicyBounds(t *testing.T) {
	for _, p := range []SessionPolicy{{}, {Access: time.Hour, Refresh: time.Hour, Idle: time.Minute, Absolute: time.Hour}} {
		if err := p.Validate(); err == nil {
			t.Fatal(fmt.Sprint("invalid policy accepted: ", p))
		}
	}
}

func TestRevocationCommitsBeforeTaskSubmission(t *testing.T) {
	s, _ := testStore(t)
	admin, workspace := loginUser(t, s, "write-admin")
	actor, _ := loginUser(t, s, "write-operator")
	q := dbgen.New(s.pool)
	if err := q.CreateMembership(t.Context(), dbgen.CreateMembershipParams{WorkspaceID: workspace, UserID: actor.ID, Role: "Operator"}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context()) //nolint:errcheck // Commit closes the transaction.
	locked := q.WithTx(tx)
	if err = authorizeTx(t.Context(), locked, admin, workspace, "members", true); err != nil {
		t.Fatal(err)
	}
	if _, err = locked.UpdateMember(t.Context(), dbgen.UpdateMemberParams{WorkspaceID: workspace, UserID: actor.ID, Role: "Viewer", Status: "revoked"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Submit(WithPrincipal(t.Context(), actor), workspace, "must-not-submit", input())
		done <- err
	}()
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrForbidden) {
		t.Fatal("revocation-first submission accepted", err)
	}
	var count int
	if err = s.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_tasks WHERE workspace_id=$1`, workspace).Scan(&count); err != nil || count != 0 {
		t.Fatal("forbidden task persisted", err)
	}
}

func TestTaskAndOutboxFailureRollBackTogether(t *testing.T) {
	s, ws := testStore(t)
	if _, err := s.pool.Exec(t.Context(), `CREATE FUNCTION fail_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END $$;
CREATE TRIGGER outbox_failure BEFORE INSERT ON makez_outbox FOR EACH ROW EXECUTE FUNCTION fail_outbox()`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Submit(testPrincipal(t.Context(), ws), ws, "atomic", input()); err == nil {
		t.Fatal("injected outbox failure succeeded")
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM makez_tasks`).Scan(&count); err != nil || count != 0 {
		t.Fatal("task survived outbox rollback", err)
	}
}

func TestDisabledAdminCannotSatisfyLastAdminInvariant(t *testing.T) {
	s, _ := testStore(t)
	active, workspace := loginUser(t, s, "active-admin")
	disabled, _ := loginUser(t, s, "disabled-admin")
	if err := dbgen.New(s.pool).CreateMembership(t.Context(), dbgen.CreateMembershipParams{WorkspaceID: workspace, UserID: disabled.ID, Role: "Admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE makez_users SET status='disabled' WHERE id=$1`, disabled.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ChangeMember(t.Context(), active, workspace, active.ID, "Viewer", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("disabled Admin counted as effective administrator: %v", err)
	}
}
