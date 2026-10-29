package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/makez-dev/makez/internal/store/dbgen"
)

// SessionPolicy contains configurable implementation defaults, not production approval.
type SessionPolicy struct{ Access, Refresh, Absolute, Idle time.Duration }

// DefaultSessionPolicy is deliberately bounded by an absolute session lifetime.
func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{Access: 10 * time.Minute, Refresh: 7 * 24 * time.Hour, Absolute: 30 * 24 * time.Hour, Idle: 7 * 24 * time.Hour}
}

// Validate rejects nonsensical or unbounded session configuration.
func (p SessionPolicy) Validate() error {
	if p.Access <= 0 || p.Refresh <= 0 || p.Absolute <= 0 || p.Idle <= 0 || p.Access > p.Idle || p.Idle > p.Absolute || p.Refresh > p.Absolute {
		return ErrInvalidInput
	}
	return nil
}

// TokenPair is returned only after the token digests have committed.
type TokenPair struct {
	Access         string
	Refresh        string
	ExpiresIn      int64
	RefreshExpires time.Time
}

func opaque(prefix string) string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b[:])
}
func tokenDigest(token string) []byte          { d := sha256.Sum256([]byte(token)); return d[:] }
func timestamp(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func issuePair(ctx context.Context, q *dbgen.Queries, session string, generation int64, now, deadline time.Time, p SessionPolicy) (TokenPair, error) {
	pair := TokenPair{Access: opaque("mza_"), Refresh: opaque("mzr_")}
	accessEnd := earlier(now.Add(p.Access), deadline)
	pair.RefreshExpires = earlier(now.Add(p.Refresh), deadline)
	pair.ExpiresIn = int64(accessEnd.Sub(now) / time.Second)
	if err := q.InsertAccess(ctx, dbgen.InsertAccessParams{ID: uuid.New().String(), SessionID: session, TokenDigest: tokenDigest(pair.Access), CreatedAt: timestamp(now), ExpiresAt: timestamp(accessEnd)}); err != nil {
		return TokenPair{}, err
	}
	if err := q.InsertRefresh(ctx, dbgen.InsertRefreshParams{ID: uuid.New().String(), SessionID: session, TokenDigest: tokenDigest(pair.Refresh), Generation: generation, CreatedAt: timestamp(now), ExpiresAt: timestamp(pair.RefreshExpires)}); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

// CreateSession locks the user before creating a new independent refresh family.
func (s *Store) CreateSession(ctx context.Context, user string, p SessionPolicy) (TokenPair, error) {
	if err := p.Validate(); err != nil {
		return TokenPair{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenPair{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	u, err := q.LockUser(ctx, user)
	if err != nil {
		return TokenPair{}, err
	}
	if u.Status != "active" {
		return TokenPair{}, ErrUnauthenticated
	}
	now := time.Now().UTC()
	id := uuid.New().String()
	absolute := now.Add(p.Absolute)
	idle := earlier(now.Add(p.Idle), absolute)
	if err = q.InsertSession(ctx, dbgen.InsertSessionParams{ID: id, UserID: user, AuthEpoch: u.AuthEpoch, CreatedAt: timestamp(now), ExpiresAt: timestamp(absolute), IdleExpiresAt: timestamp(idle)}); err != nil {
		return TokenPair{}, err
	}
	pair, err := issuePair(ctx, q, id, 0, now, idle, p)
	if err != nil {
		return TokenPair{}, err
	}
	if err = audit(ctx, q, "", Principal{Kind: "user", ID: user}, "login", "allowed", id); err != nil {
		return TokenPair{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

// AuthenticateAccess consults token, session, epoch and user status on every call.
func (s *Store) AuthenticateAccess(ctx context.Context, token string) (Principal, error) {
	if !strings.HasPrefix(token, "mza_") || len(token) != 47 {
		return Principal{}, ErrUnauthenticated
	}
	id, err := dbgen.New(s.pool).AuthenticateAccess(ctx, dbgen.AuthenticateAccessParams{TokenDigest: tokenDigest(token), ExpiresAt: timestamp(time.Now())})
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{Kind: "user", ID: id}, err
}

// Refresh strictly consumes one token; replay revokes the entire family,
// including access tokens issued by a concurrent successful request.
func (s *Store) Refresh(ctx context.Context, token string, p SessionPolicy) (TokenPair, error) {
	if err := p.Validate(); err != nil {
		return TokenPair{}, err
	}
	if !strings.HasPrefix(token, "mzr_") || len(token) != 47 {
		return TokenPair{}, ErrUnauthenticated
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenPair{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	ref, err := q.FindRefreshSession(ctx, tokenDigest(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, ErrUnauthenticated
	}
	if err != nil {
		return TokenPair{}, err
	}
	// Universal session lock order: user, session, refresh token.
	u, err := q.LockUser(ctx, ref.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	session, err := q.LockSession(ctx, ref.ID)
	if err != nil {
		return TokenPair{}, err
	}
	r, err := q.LockRefresh(ctx, tokenDigest(token))
	if err != nil {
		return TokenPair{}, err
	}
	now := time.Now().UTC()
	if r.UsedAt.Valid {
		if err = q.RevokeSession(ctx, dbgen.RevokeSessionParams{ID: session.ID, RevokedAt: timestamp(now)}); err != nil {
			return TokenPair{}, err
		}
		if err = audit(ctx, q, "", Principal{Kind: "user", ID: u.ID}, "refresh_replay", "denied", session.ID); err != nil {
			return TokenPair{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return TokenPair{}, err
		}
		return TokenPair{}, ErrUnauthenticated
	}
	if u.Status != "active" || u.AuthEpoch != session.AuthEpoch || session.RevokedAt.Valid || r.RevokedAt.Valid || !now.Before(r.ExpiresAt.Time) || !now.Before(session.ExpiresAt.Time) || !now.Before(session.IdleExpiresAt.Time) {
		return TokenPair{}, ErrUnauthenticated
	}
	if err = q.ConsumeRefresh(ctx, dbgen.ConsumeRefreshParams{ID: r.ID, UsedAt: timestamp(now)}); err != nil {
		return TokenPair{}, err
	}
	idle := earlier(now.Add(p.Idle), session.ExpiresAt.Time)
	if err = q.TouchSession(ctx, dbgen.TouchSessionParams{ID: session.ID, LastSeenAt: timestamp(now), IdleExpiresAt: timestamp(idle)}); err != nil {
		return TokenPair{}, err
	}
	pair, err := issuePair(ctx, q, session.ID, r.Generation+1, now, idle, p)
	if err != nil {
		return TokenPair{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return TokenPair{}, err
	}
	return pair, nil
}

// Logout requires only the session's refresh cookie, even after access expiration.
func (s *Store) Logout(ctx context.Context, refresh string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	ref, err := q.FindRefreshSession(ctx, tokenDigest(refresh))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = q.LockUser(ctx, ref.UserID); err != nil {
		return err
	}
	if err = q.RevokeSession(ctx, dbgen.RevokeSessionParams{ID: ref.ID, RevokedAt: timestamp(time.Now())}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LogoutAll invalidates all sessions without changing workspace membership or tasks.
func (s *Store) LogoutAll(ctx context.Context, p Principal) error {
	if p.Kind != "user" {
		return ErrForbidden
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // Commit closes the transaction.
	q := dbgen.New(s.pool).WithTx(tx)
	if _, err = q.LockUser(ctx, p.ID); err != nil {
		return err
	}
	if err = q.BumpUserEpoch(ctx, p.ID); err != nil {
		return err
	}
	if err = q.RevokeAllSessions(ctx, dbgen.RevokeAllSessionsParams{UserID: p.ID, RevokedAt: timestamp(time.Now())}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LoginTransaction is a single-use, short-lived browser binding for OIDC.
type LoginTransaction struct{ State, Browser, Nonce, Verifier string }

// StartLogin stores only state/browser digests. The PKCE verifier expires in five minutes.
func (s *Store) StartLogin(ctx context.Context) (LoginTransaction, error) {
	l := LoginTransaction{State: opaque(""), Browser: opaque(""), Nonce: opaque(""), Verifier: opaque("")}
	err := dbgen.New(s.pool).CreateLoginTransaction(ctx, dbgen.CreateLoginTransactionParams{StateDigest: tokenDigest(l.State), BrowserDigest: tokenDigest(l.Browser), Nonce: l.Nonce, Verifier: l.Verifier, ExpiresAt: timestamp(time.Now().Add(5 * time.Minute))})
	return l, err
}

// ConsumeLogin atomically prevents callback replay, including failed exchanges.
func (s *Store) ConsumeLogin(ctx context.Context, state, browser string) (LoginTransaction, error) {
	if len(state) != 43 || len(browser) != 43 {
		return LoginTransaction{}, ErrUnauthenticated
	}
	l, err := dbgen.New(s.pool).ConsumeLoginTransaction(ctx, dbgen.ConsumeLoginTransactionParams{StateDigest: tokenDigest(state), BrowserDigest: tokenDigest(browser)})
	if errors.Is(err, pgx.ErrNoRows) {
		return LoginTransaction{}, ErrUnauthenticated
	}
	return LoginTransaction{Nonce: l.Nonce, Verifier: l.Verifier}, err
}
