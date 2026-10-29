package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/makez-dev/makez/internal/store/dbgen"
)

// Authentication and authorization failures are deliberately distinct.
var (
	ErrUnauthenticated = errors.New("authentication required")
	ErrForbidden       = errors.New("permission denied")
)

// Principal is a verified caller identity; it never contains credentials.
type Principal struct{ Kind, ID string }
type principalKey struct{}
type requestIDKey struct{}

// WithRequestID carries the sanitized HTTP request identity into append-only audit records.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// WithPrincipal carries a previously authenticated identity across service calls.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// CurrentPrincipal returns the verified caller, if present.
func CurrentPrincipal(ctx context.Context) Principal {
	p, _ := ctx.Value(principalKey{}).(Principal)
	return p
}

// Identity contains verified external claims, never values accepted from a business request.
type Identity struct {
	Issuer, Subject, Email, Name string
	EmailVerified                bool
}

// User is the minimal public user profile.
type User struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// Workspace is one current membership visible to the caller.
type Workspace struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Role    string `json:"role"`
	Version int64  `json:"authz_version"`
}

// Member is an active workspace member.
type Member struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// LoginIdentity atomically creates the first user's default workspace and Admin membership.
func (s *Store) LoginIdentity(ctx context.Context, in Identity) (User, error) {
	if in.Issuer != "https://accounts.google.com" || in.Subject == "" || len(in.Subject) > 255 {
		return User{}, ErrUnauthenticated
	}
	for range 3 {
		user, err := s.loginIdentity(ctx, in)
		if pgerr, ok := errors.AsType[*pgconn.PgError](err); ok && pgerr.Code == "23505" {
			continue
		}
		return user, err
	}
	return User{}, ErrConflict
}
func (s *Store) loginIdentity(ctx context.Context, in Identity) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	found, err := q.FindIdentity(ctx, dbgen.FindIdentityParams{Issuer: in.Issuer, Subject: in.Subject})
	if err == nil {
		if found.Status != "active" {
			return User{}, ErrUnauthenticated
		}
		if err = q.UpdateIdentity(ctx, dbgen.UpdateIdentityParams{Issuer: in.Issuer, Subject: in.Subject, Email: in.Email, EmailVerified: in.EmailVerified}); err != nil {
			return User{}, err
		}
		return User{ID: found.ID, DisplayName: found.DisplayName}, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, err
	}
	id, workspace := uuid.New().String(), uuid.New().String()
	if err = q.CreateUser(ctx, dbgen.CreateUserParams{ID: id, DisplayName: in.Name}); err != nil {
		return User{}, err
	}
	if err = q.CreateIdentity(ctx, dbgen.CreateIdentityParams{ID: uuid.New().String(), UserID: id, Issuer: in.Issuer, Subject: in.Subject, Email: in.Email, EmailVerified: in.EmailVerified}); err != nil {
		return User{}, err
	}
	if err = q.CreateWorkspace(ctx, dbgen.CreateWorkspaceParams{ID: workspace, Slug: "workspace-" + workspace, BootstrapUserID: new(id)}); err != nil {
		return User{}, err
	}
	if err = q.CreateMembership(ctx, dbgen.CreateMembershipParams{WorkspaceID: workspace, UserID: id, Role: "Admin"}); err != nil {
		return User{}, err
	}
	if err = audit(ctx, q, workspace, Principal{Kind: "user", ID: id}, "register", "allowed", id); err != nil {
		return User{}, err
	}
	return User{ID: id, DisplayName: in.Name}, tx.Commit(ctx)
}

func audit(ctx context.Context, q *dbgen.Queries, ws string, p Principal, action, outcome, resource string) error {
	var workspace *string
	if ws != "" {
		workspace = new(ws)
	}
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return q.Audit(ctx, dbgen.AuditParams{ID: uuid.New().String(), WorkspaceID: workspace, ActorKind: p.Kind, ActorID: p.ID, Action: action, Outcome: outcome, ResourceID: resource, RequestID: requestID})
}

// Me loads the caller's current public profile.
func (s *Store) Me(ctx context.Context, p Principal) (User, error) {
	if p.Kind != "user" {
		return User{}, ErrForbidden
	}
	u, err := dbgen.New(s.pool).GetUser(ctx, p.ID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && u.Status != "active") {
		return User{}, ErrUnauthenticated
	}
	return User{ID: u.ID, DisplayName: u.DisplayName}, err
}

// Workspaces exposes only current, active memberships.
func (s *Store) Workspaces(ctx context.Context, p Principal) ([]Workspace, error) {
	if p.Kind != "user" {
		return nil, ErrForbidden
	}
	rows, err := dbgen.New(s.pool).ListWorkspaces(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	result := make([]Workspace, 0, len(rows))
	for _, r := range rows {
		result = append(result, Workspace{ID: r.ID, Slug: r.Slug, Role: r.Role, Version: r.AuthzVersion})
	}
	return result, nil
}

// RegisterMachines materializes explicitly configured keys without granting any
// historical task an inferred owner. Revoked rows are never reactivated.
func (s *Store) RegisterMachines(ctx context.Context, keys map[string]string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	for key, ws := range keys {
		if len(key) < 16 || ws == "" || len(ws) > 128 || strings.HasPrefix(key, "mza_") || strings.HasPrefix(key, "mzr_") {
			return ErrInvalidInput
		}
		digest := sha256.Sum256([]byte(key))
		id := "machine-" + hex.EncodeToString(digest[:])
		if err = dbgen.New(tx).RegisterMachineWorkspace(ctx, dbgen.RegisterMachineWorkspaceParams{ID: ws, Slug: "machine-" + hex.EncodeToString(tokenDigest(ws))}); err != nil {
			return err
		}
		if err = dbgen.New(tx).RegisterMachine(ctx, dbgen.RegisterMachineParams{ID: id, WorkspaceID: ws, CredentialDigest: digest[:]}); err != nil {
			return err
		}
		bound, err := dbgen.New(tx).MachineWorkspace(ctx, id)
		if err != nil {
			return err
		}
		if bound != ws {
			return errors.New("machine key workspace binding cannot change")
		}
	}
	return tx.Commit(ctx)
}

// AuthenticateMachine resolves an opaque machine credential with no user-token fallback.
func (s *Store) AuthenticateMachine(ctx context.Context, token string) (Principal, string, error) {
	digest := sha256.Sum256([]byte(token))
	row, err := dbgen.New(s.pool).FindMachine(ctx, digest[:])
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, "", ErrUnauthenticated
	}
	return Principal{Kind: "machine", ID: row.ID}, row.WorkspaceID, err
}

func roleAllows(role, workspace, action string) (bool, error) {
	// The ephemeral domain model is built exclusively from the current database role.
	// No Casbin adapter or mutable second membership store exists.
	m, err := model.NewModelFromString(`[request_definition]
r = sub, dom, act
[policy_definition]
p = sub, dom, act
[role_definition]
g = _, _, _
[policy_effect]
e = some(where (p.eft == allow))
[matchers]
m = g(r.sub, p.sub, r.dom) && r.dom == p.dom && r.act == p.act`)
	if err != nil {
		return false, err
	}
	e, err := casbin.NewEnforcer(m)
	if err != nil {
		return false, err
	}
	if _, err = e.AddGroupingPolicy("caller", role, workspace); err != nil {
		return false, err
	}
	for _, r := range []string{"Admin", "Operator", "Viewer"} {
		if _, err = e.AddPolicy(r, workspace, "read"); err != nil {
			return false, err
		}
		if r != "Viewer" {
			if _, err = e.AddPolicy(r, workspace, "write"); err != nil {
				return false, err
			}
		}
	}
	if _, err = e.AddPolicy("Admin", workspace, "members"); err != nil {
		return false, err
	}
	return e.Enforce("caller", workspace, action)
}

func authorizeTx(ctx context.Context, q *dbgen.Queries, p Principal, workspace, action string, exclusive bool) error {
	var status string
	var err error
	if exclusive {
		status, err = q.WorkspaceExclusiveLock(ctx, workspace)
	} else {
		status, err = q.WorkspaceShareLock(ctx, workspace)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != "active" {
		return ErrForbidden
	}
	var role string
	switch p.Kind {
	case "user":
		role, err = q.UserRole(ctx, dbgen.UserRoleParams{WorkspaceID: workspace, UserID: p.ID})
	case "machine":
		role, err = q.MachineRole(ctx, dbgen.MachineRoleParams{WorkspaceID: workspace, ID: p.ID})
	default:
		return ErrForbidden
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	ok, err := roleAllows(role, workspace, action)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return nil
}

// Authorize always consults the current workspace and membership state.
func (s *Store) Authorize(ctx context.Context, p Principal, workspace, action string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Read locks are released after authorization.
	err = authorizeTx(ctx, dbgen.New(s.pool).WithTx(tx), p, workspace, action, false)
	outcome := "allowed"
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound) {
		outcome = "denied"
	} else if err != nil {
		outcome = "error"
	}
	s.Metrics.Authorization(ctx, action, outcome)
	return err
}

// Members returns workspace members to a currently authorized Admin.
func (s *Store) Members(ctx context.Context, p Principal, workspace string) ([]Member, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Read transaction only.
	q := dbgen.New(s.pool).WithTx(tx)
	if err = authorizeTx(ctx, q, p, workspace, "members", false); err != nil {
		return nil, err
	}
	rows, err := q.ListMembers(ctx, workspace)
	if err != nil {
		return nil, err
	}
	result := make([]Member, 0, len(rows))
	for _, r := range rows {
		result = append(result, Member{UserID: r.UserID, DisplayName: r.DisplayName, Role: r.Role})
	}
	return result, nil
}

// ChangeMember serializes membership writes at the workspace boundary.
func (s *Store) ChangeMember(ctx context.Context, p Principal, workspace, user, role string, revoke bool) error {
	if role != "Admin" && role != "Operator" && role != "Viewer" {
		return ErrInvalidInput
	}
	if _, err := uuid.Parse(user); err != nil {
		return ErrInvalidInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	if err = authorizeTx(ctx, q, p, workspace, "members", true); err != nil {
		return err
	}
	members, err := q.ListMembers(ctx, workspace)
	if err != nil {
		return err
	}
	old := ""
	for _, m := range members {
		if m.UserID == user {
			old = m.Role
		}
	}
	if old == "" {
		return ErrNotFound
	}
	if old == "Admin" && (role != "Admin" || revoke) {
		count, e := q.CountAdmins(ctx, workspace)
		if e != nil {
			return e
		}
		if count <= 1 {
			return ErrConflict
		}
	}
	status := "active"
	if revoke {
		status = "revoked"
	}
	if _, err = q.UpdateMember(ctx, dbgen.UpdateMemberParams{WorkspaceID: workspace, UserID: user, Role: role, Status: status}); err != nil {
		return err
	}
	version, err := q.BumpAuthorization(ctx, workspace)
	if err != nil {
		return err
	}
	if err = q.EnqueueAuthorization(ctx, dbgen.EnqueueAuthorizationParams{ID: uuid.New().String(), WorkspaceID: workspace, AuthzVersion: version}); err != nil {
		return err
	}
	if err = audit(ctx, q, workspace, p, "membership_changed", "allowed", user); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AdmitTaskAction rechecks the original principal, independent of login sessions.
// A committed audit row is the acceptance boundary before external side effects.
func (s *Store) AdmitTaskAction(ctx context.Context, workspace, task, action string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	actor, err := q.GetTaskActor(ctx, dbgen.GetTaskActorParams{WorkspaceID: workspace, ID: task})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	p := Principal{Kind: "unknown"}
	if actor.SubmittedByUserID != nil {
		p = Principal{Kind: "user", ID: *actor.SubmittedByUserID}
	}
	if actor.SubmittedByMachineID != nil {
		p = Principal{Kind: "machine", ID: *actor.SubmittedByMachineID}
	}
	err = authorizeTx(ctx, q, p, workspace, "write", false)
	if err != nil && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrNotFound) {
		return err
	}
	outcome := "allowed"
	if err != nil {
		outcome = "denied"
	}
	if e := audit(ctx, q, workspace, p, action, outcome, task); e != nil {
		return e
	}
	if e := tx.Commit(ctx); e != nil {
		return e
	}
	s.Metrics.Authorization(ctx, "worker", outcome)
	if err != nil {
		return fmt.Errorf("task principal cannot admit %s: %w", action, err)
	}
	return nil
}
