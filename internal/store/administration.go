package store

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/store/dbgen"
)

// RevokeMachine is an explicit operator command. Key removal from API configuration
// stops API authentication; this durable revocation also fences queued worker actions.
func (s *Store) RevokeMachine(ctx context.Context, workspace, id string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	if _, err = q.WorkspaceExclusiveLock(ctx, workspace); err != nil {
		return err
	}
	count, err := q.RevokeMachine(ctx, dbgen.RevokeMachineParams{WorkspaceID: workspace, ID: id})
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	version, err := q.BumpAuthorization(ctx, workspace)
	if err != nil {
		return err
	}
	if err = q.EnqueueAuthorization(ctx, dbgen.EnqueueAuthorizationParams{ID: uuid.New().String(), WorkspaceID: workspace, AuthzVersion: version}); err != nil {
		return err
	}
	if err = audit(ctx, q, workspace, Principal{Kind: "operator", ID: "migration-command"}, "revoke_machine", "allowed", id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RepairBootstrap explicitly repairs an imported user's missing default workspace.
// It is not called by login and is safe to repeat.
func (s *Store) RepairBootstrap(ctx context.Context, user string) error {
	if _, err := uuid.Parse(user); err != nil {
		return ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	if _, err = q.LockUser(ctx, user); err != nil {
		return err
	}
	if _, err = q.FindBootstrap(ctx, new(user)); err == nil {
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	workspace := uuid.New().String()
	if err = q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{ID: workspace, Slug: "workspace-" + workspace, BootstrapUserID: new(user)}); err != nil {
		return err
	}
	if err = q.CreateMembership(ctx, dbgen.CreateMembershipParams{WorkspaceID: workspace, UserID: user, Role: "Admin"}); err != nil {
		return err
	}
	if err = audit(ctx, q, workspace, Principal{Kind: "operator", ID: "migration-command"}, "repair_bootstrap", "allowed", user); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AuthorizationChange is a notification, never an independent role fact source.
type AuthorizationChange struct {
	ID, WorkspaceID, Owner string
	Version, LeaseToken    int64
}

// ClaimAuthorization claims invalidation notifications with the outbox fencing protocol.
func (s *Store) ClaimAuthorization(ctx context.Context) ([]AuthorizationChange, error) {
	rows, err := dbgen.New(s.pool).ClaimAuthorization(ctx, new(newID()))
	if err != nil {
		return nil, err
	}
	result := make([]AuthorizationChange, 0, len(rows))
	for _, r := range rows {
		result = append(result, AuthorizationChange{ID: r.ID, WorkspaceID: r.WorkspaceID, Owner: *r.LeaseOwner, Version: r.AuthzVersion, LeaseToken: r.LeaseToken})
	}
	return result, nil
}

// FinishAuthorization acknowledges or retries only the current lease.
func (s *Store) FinishAuthorization(ctx context.Context, change AuthorizationChange, published bool) error {
	q := dbgen.New(s.pool)
	var count int64
	var err error
	if published {
		count, err = q.AckAuthorization(ctx, dbgen.AckAuthorizationParams{ID: change.ID, LeaseOwner: new(change.Owner), LeaseToken: change.LeaseToken})
	} else {
		count, err = q.RetryAuthorization(ctx, dbgen.RetryAuthorizationParams{ID: change.ID, LeaseOwner: new(change.Owner), LeaseToken: change.LeaseToken})
	}
	if err == nil && count != 1 {
		return ErrStale
	}
	return err
}
