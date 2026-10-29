package store

import (
	"errors"
	"testing"
)

func TestOutboxLeaseFencesStaleDispatcher(t *testing.T) {
	s, ws := testStore(t)
	submit(t, s, ws)
	old, err := s.Pending(t.Context(), 1)
	if err != nil || len(old) != 1 {
		t.Fatalf("claim: %v %v", old, err)
	}
	other, err := s.Pending(t.Context(), 1)
	if err != nil || len(other) != 0 {
		t.Fatalf("concurrent claim: %v %v", other, err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE makez_outbox SET lease_until=now()-interval '1 second' WHERE id=$1`, old[0].ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.Pending(t.Context(), 1)
	if err != nil || len(current) != 1 || current[0].LeaseToken <= old[0].LeaseToken {
		t.Fatal("lease did not advance", err)
	}
	if err = s.Delivered(t.Context(), old[0]); !errors.Is(err, ErrStale) {
		t.Fatalf("stale ack: %v", err)
	}
	if err = s.RetryOutbox(t.Context(), old[0], "stale"); !errors.Is(err, ErrStale) {
		t.Fatalf("stale retry: %v", err)
	}
	if err = s.Delivered(t.Context(), current[0]); err != nil {
		t.Fatal(err)
	}
}

func TestCompositeWorkspaceConstraint(t *testing.T) {
	s, ws := testStore(t)
	task := submit(t, s, ws)
	_, err := s.pool.Exec(t.Context(), `INSERT INTO makez_runs(id,task_id,workspace_id,state,backend_id) VALUES('foreign',$1,'another','PREPARING','test')`, task.ID)
	if err == nil {
		t.Fatal("cross-workspace run accepted")
	}
	_, err = s.pool.Exec(t.Context(), `INSERT INTO makez_outbox(id,task_id,workspace_id,command) VALUES('foreign',$1,'another','CANCEL_TASK')`, task.ID)
	if err == nil {
		t.Fatal("cross-workspace outbox accepted")
	}
}

func TestDeadLetterReplayRetainsEventIdentity(t *testing.T) {
	s, workspace := testStore(t)
	submit(t, s, workspace)
	if _, err := s.pool.Exec(t.Context(), `UPDATE makez_outbox SET attempts=11`); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Pending(t.Context(), 1)
	if err != nil || len(claimed) != 1 {
		t.Fatal(err)
	}
	if err = s.RetryOutbox(t.Context(), claimed[0], "test failure"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Pending(t.Context(), 1); err != nil || len(rows) != 0 {
		t.Fatal("dead letter reclaimed automatically", err)
	}
	if err = s.ReplayOutbox(t.Context(), "foreign", claimed[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-workspace replay", err)
	}
	if err = s.ReplayOutbox(t.Context(), workspace, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	replayed, err := s.Pending(t.Context(), 1)
	if err != nil || len(replayed) != 1 || replayed[0].ID != claimed[0].ID || replayed[0].LeaseToken <= claimed[0].LeaseToken {
		t.Fatal("replay changed identity or fence", err)
	}
}

func TestAuthorizationOutboxFencesStaleAcknowledgment(t *testing.T) {
	s, ws := testStore(t)
	p, _, err := s.AuthenticateMachine(t.Context(), "test-key-"+ws)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeMachine(t.Context(), ws, p.ID); err != nil {
		t.Fatal(err)
	}
	old, err := s.ClaimAuthorization(t.Context())
	if err != nil || len(old) != 1 {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE makez_authz_outbox SET lease_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	current, err := s.ClaimAuthorization(t.Context())
	if err != nil || len(current) != 1 {
		t.Fatal(err)
	}
	if err = s.FinishAuthorization(t.Context(), old[0], true); !errors.Is(err, ErrStale) {
		t.Fatal("stale authorization ack", err)
	}
	if err = s.FinishAuthorization(t.Context(), current[0], true); err != nil {
		t.Fatal(err)
	}
}
