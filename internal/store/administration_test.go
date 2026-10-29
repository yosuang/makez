package store

import (
	"errors"
	"testing"
	"uuid"

	"github.com/makez-dev/makez/internal/store/dbgen"
)

func TestExplicitBootstrapRepairIsIdempotent(t *testing.T) {
	s, _ := testStore(t)
	q := dbgen.New(s.pool)
	user := uuid.New().String()
	if err := q.CreateUser(t.Context(), dbgen.CreateUserParams{ID: user, DisplayName: "Imported"}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateIdentity(t.Context(), dbgen.CreateIdentityParams{ID: uuid.New().String(), UserID: user, Issuer: "https://accounts.google.com", Subject: "imported"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoginIdentity(t.Context(), Identity{Issuer: "https://accounts.google.com", Subject: "imported"})
	if err != nil || got.ID != user {
		t.Fatal("imported identity changed", err)
	}
	p := Principal{Kind: "user", ID: user}
	before, err := s.Workspaces(t.Context(), p)
	if err != nil || len(before) != 0 {
		t.Fatal("login silently repaired historical bootstrap", err)
	}
	for range 2 {
		if err = s.RepairBootstrap(t.Context(), user); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.Workspaces(t.Context(), p)
	if err != nil || len(after) != 1 || after[0].Role != "Admin" {
		t.Fatal("repair was not idempotent", err)
	}
	profile, err := s.Me(t.Context(), p)
	if err != nil || profile.ID != user || profile.DisplayName != "Imported" {
		t.Fatal("profile changed", err)
	}
}

func TestAuthorizationRetryDeadLettersAndFences(t *testing.T) {
	s, workspace := testStore(t)
	p, _, err := s.AuthenticateMachine(t.Context(), "test-key-"+workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RevokeMachine(t.Context(), workspace, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.pool.Exec(t.Context(), `UPDATE makez_authz_outbox SET attempts=11`); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ClaimAuthorization(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatal("claim", err)
	}
	if err = s.FinishAuthorization(t.Context(), rows[0], false); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishAuthorization(t.Context(), rows[0], false); !errors.Is(err, ErrStale) {
		t.Fatal("repeated retry accepted", err)
	}
	pending, err := s.ClaimAuthorization(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatal("dead authorization notification reclaimed", err)
	}
	var attempts int
	var dead bool
	if err = s.pool.QueryRow(t.Context(), `SELECT attempts,dead_at IS NOT NULL FROM makez_authz_outbox WHERE id=$1`, rows[0].ID).Scan(&attempts, &dead); err != nil || attempts != 12 || !dead {
		t.Fatal("retry not bounded", err)
	}
}
