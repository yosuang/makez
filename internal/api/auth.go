package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/labstack/echo/v5"
	"github.com/makez-dev/makez/internal/store"
	"golang.org/x/net/publicsuffix"
	"golang.org/x/oauth2"
)

// GoogleConfig pins the redirect and frontend origins; credentials are supplied by environment configuration.
type GoogleConfig struct {
	ClientID, ClientSecret, RedirectURL, FrontendOrigin string
	Development                                         bool
}

// GoogleAuth implements authorization code with PKCE and verified OIDC claims.
type GoogleAuth struct {
	config   GoogleConfig
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// NewGoogleAuth discovers only Google's canonical issuer. Tests use a local provider internally.
func NewGoogleAuth(ctx context.Context, c GoogleConfig) (*GoogleAuth, error) {
	return newGoogleAuth(ctx, c, "https://accounts.google.com")
}
func newGoogleAuth(ctx context.Context, c GoogleConfig, issuer string) (*GoogleAuth, error) {
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, errors.New("google client ID and secret are required")
	}
	front, err := url.Parse(c.FrontendOrigin)
	if err != nil || front.Host == "" || front.User != nil || front.RawQuery != "" || front.Fragment != "" || front.Path != "" || (front.Scheme != "https" && (!c.Development || front.Scheme != "http" || !loopbackHost(front.Hostname()))) {
		return nil, errors.New("invalid frontend origin")
	}
	callback, err := url.Parse(c.RedirectURL)
	if err != nil || callback.Host == "" || callback.User != nil || callback.Path != "/v1/auth/google/callback" || callback.RawQuery != "" || callback.Fragment != "" || (callback.Scheme != "https" && (!c.Development || callback.Scheme != "http" || !loopbackHost(callback.Hostname()))) {
		return nil, errors.New("invalid Google callback URL")
	}
	if !sameSite(front, callback) {
		return nil, errors.New("cross-site browser authentication requires a separately approved cookie or BFF design")
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	return &GoogleAuth{config: c, oauth: oauth2.Config{ClientID: c.ClientID, ClientSecret: c.ClientSecret, RedirectURL: c.RedirectURL, Endpoint: provider.Endpoint(), Scopes: []string{oidc.ScopeOpenID, "email", "profile"}}, verifier: provider.Verifier(&oidc.Config{ClientID: c.ClientID})}, nil
}
func loopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func sameSite(front, callback *url.URL) bool {
	if front.Scheme != callback.Scheme {
		return false
	}
	if strings.EqualFold(front.Hostname(), callback.Hostname()) {
		return true
	}
	left, leftErr := publicsuffix.EffectiveTLDPlusOne(front.Hostname())
	right, rightErr := publicsuffix.EffectiveTLDPlusOne(callback.Hostname())
	return leftErr == nil && rightErr == nil && strings.EqualFold(left, right)
}

func (s *Server) cookieName(kind string) string {
	if s.Development {
		return "makez-" + kind
	}
	return "__Host-makez-" + kind
}
func (s *Server) setAuthCookie(c *echo.Context, kind, value string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if value == "" {
		maxAge = -1
	}
	http.SetCookie(c.Response(), &http.Cookie{Name: s.cookieName(kind), Value: value, Path: "/", HttpOnly: true, Secure: !s.Development, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge})
}
func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}
func (s *Server) csrf(c *echo.Context) error {
	if !s.originAllowed(c.Request().Header.Get("Origin")) || c.Request().Header.Get("X-Makez-CSRF") != "1" {
		return echo.NewHTTPError(403, "request origin or CSRF header rejected")
	}
	return nil
}
func (s *Server) policy() store.SessionPolicy {
	if s.SessionPolicy.Absolute == 0 {
		return store.DefaultSessionPolicy()
	}
	return s.SessionPolicy
}

// googleStart begins a browser-bound login transaction.
// @Summary Sign in with Google
// @Tags auth
// @Success 302
// @Failure 400,503 {object} ErrorResponse
// @Router /auth/google/start [get]
func (s *Server) googleStart(c *echo.Context) error {
	if s.Google == nil {
		return echo.NewHTTPError(503, "Google login is not configured")
	}
	if path := c.QueryParam("return_to"); path != "" && path != "/" {
		return echo.NewHTTPError(400, "unsupported return path")
	}
	l, err := s.DB.StartLogin(c.Request().Context())
	if err != nil {
		return err
	}
	s.setAuthCookie(c, "login", l.Browser, time.Now().Add(5*time.Minute))
	return c.Redirect(http.StatusFound, s.Google.oauth.AuthCodeURL(l.State, oidc.Nonce(l.Nonce), oauth2.S256ChallengeOption(l.Verifier)))
}

// googleCallback validates and consumes the one-time OIDC transaction.
// @Summary Complete Google login
// @Tags auth
// @Success 303
// @Failure 401,503 {object} ErrorResponse
// @Router /auth/google/callback [get]
func (s *Server) googleCallback(c *echo.Context) error {
	if s.Google == nil {
		return echo.NewHTTPError(503, "Google login is not configured")
	}
	browser, err := c.Request().Cookie(s.cookieName("login"))
	if err != nil {
		return store.ErrUnauthenticated
	}
	l, err := s.DB.ConsumeLogin(c.Request().Context(), c.QueryParam("state"), browser.Value)
	s.setAuthCookie(c, "login", "", time.Unix(1, 0))
	if err != nil {
		return err
	}
	code := c.QueryParam("code")
	if code == "" || len(code) > 4096 {
		return store.ErrUnauthenticated
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 10*time.Second)
	defer cancel()
	tokens, err := s.Google.oauth.Exchange(ctx, code, oauth2.VerifierOption(l.Verifier))
	if err != nil {
		return store.ErrUnauthenticated
	}
	raw, ok := tokens.Extra("id_token").(string)
	if !ok {
		return store.ErrUnauthenticated
	}
	id, err := s.Google.verifier.Verify(ctx, raw)
	if err != nil || id.Nonce != l.Nonce || id.Subject == "" {
		return store.ErrUnauthenticated
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err = id.Claims(&claims); err != nil {
		return store.ErrUnauthenticated
	}
	user, err := s.DB.LoginIdentity(ctx, store.Identity{Issuer: "https://accounts.google.com", Subject: id.Subject, Email: claims.Email, EmailVerified: claims.EmailVerified, Name: claims.Name})
	if err != nil {
		return err
	}
	pair, err := s.DB.CreateSession(ctx, user.ID, s.policy())
	if err != nil {
		return err
	}
	s.setAuthCookie(c, "refresh", pair.Refresh, pair.RefreshExpires)
	return c.Redirect(http.StatusSeeOther, s.Google.config.FrontendOrigin+"/")
}

// AccessResponse never includes a refresh token; the browser keeps access in memory.
type AccessResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// refresh exchanges the HttpOnly cookie for a new token pair.
// @Summary Refresh a browser session
// @Tags auth
// @Success 200 {object} AccessResponse
// @Failure 401,403,503 {object} ErrorResponse
// @Router /auth/refresh [post]
func (s *Server) refresh(c *echo.Context) error {
	if err := s.csrf(c); err != nil {
		return err
	}
	cookie, err := c.Request().Cookie(s.cookieName("refresh"))
	if err != nil {
		return store.ErrUnauthenticated
	}
	pair, err := s.DB.Refresh(c.Request().Context(), cookie.Value, s.policy())
	if err != nil {
		if errors.Is(err, store.ErrUnauthenticated) {
			s.setAuthCookie(c, "refresh", "", time.Unix(1, 0))
		}
		return err
	}
	s.setAuthCookie(c, "refresh", pair.Refresh, pair.RefreshExpires)
	return c.JSON(200, AccessResponse{AccessToken: pair.Access, TokenType: "Bearer", ExpiresIn: pair.ExpiresIn})
}

// logout revokes this browser session even if access has expired.
// @Summary Sign out this session
// @Tags auth
// @Success 204
// @Failure 403,503 {object} ErrorResponse
// @Router /auth/logout [post]
func (s *Server) logout(c *echo.Context) error {
	if err := s.csrf(c); err != nil {
		return err
	}
	if cookie, err := c.Request().Cookie(s.cookieName("refresh")); err == nil {
		if err = s.DB.Logout(c.Request().Context(), cookie.Value); err != nil {
			return err
		}
	}
	s.setAuthCookie(c, "refresh", "", time.Unix(1, 0))
	return c.NoContent(204)
}

// logoutAll revokes all sessions of the authenticated user.
// @Summary Sign out all sessions
// @Tags auth
// @Security BearerAuth
// @Success 204
// @Failure 401,403,503 {object} ErrorResponse
// @Router /auth/logout-all [post]
func (s *Server) logoutAll(c *echo.Context) error {
	if err := s.csrf(c); err != nil {
		return err
	}
	if err := s.DB.LogoutAll(c.Request().Context(), store.CurrentPrincipal(c.Request().Context())); err != nil {
		return err
	}
	s.setAuthCookie(c, "refresh", "", time.Unix(1, 0))
	return c.NoContent(204)
}

// me returns the current human profile.
// @Summary Get current user
// @Tags auth
// @Security BearerAuth
// @Success 200 {object} store.User
// @Failure 401,403 {object} ErrorResponse
// @Router /me [get]
func (s *Server) me(c *echo.Context) error {
	u, err := s.DB.Me(c.Request().Context(), store.CurrentPrincipal(c.Request().Context()))
	if err != nil {
		return err
	}
	return c.JSON(200, u)
}

// workspaces lists only joined workspaces.
// @Summary List joined workspaces
// @Tags workspaces
// @Security BearerAuth
// @Success 200 {array} store.Workspace
// @Failure 401,403 {object} ErrorResponse
// @Router /workspaces [get]
func (s *Server) workspaces(c *echo.Context) error {
	w, err := s.DB.Workspaces(c.Request().Context(), store.CurrentPrincipal(c.Request().Context()))
	if err != nil {
		return err
	}
	return c.JSON(200, w)
}
func (s *Server) members(c *echo.Context) error {
	ctx := c.Request().Context()
	p := store.CurrentPrincipal(ctx)
	ws := c.Param("workspaceID")
	if c.Request().Method == http.MethodGet {
		result, err := s.DB.Members(ctx, p, ws)
		if err != nil {
			return err
		}
		return c.JSON(200, result)
	}
	revoke := c.Request().Method == http.MethodDelete
	body := struct {
		Role string `json:"role"`
	}{Role: "Viewer"}
	if !revoke {
		data, err := io.ReadAll(io.LimitReader(c.Request().Body, 1025))
		if err != nil || len(data) > 1024 {
			return echo.NewHTTPError(400, "invalid member change")
		}
		if err := json.Unmarshal(data, &body, json.RejectUnknownMembers(true)); err != nil {
			return echo.NewHTTPError(400, "invalid member change")
		}
	}
	if err := s.DB.ChangeMember(ctx, p, ws, c.Param("userID"), body.Role, revoke); err != nil {
		return err
	}
	return c.NoContent(204)
}

// listMembers lists current members for an Admin.
// @Summary List workspace members
// @Tags workspaces
// @Security BearerAuth
// @Param workspaceID path string true "Workspace ID"
// @Success 200 {array} store.Member
// @Failure 401,403,404,503 {object} ErrorResponse
// @Router /workspaces/{workspaceID}/members [get]
func (s *Server) listMembers(c *echo.Context) error { return s.members(c) }

// MemberChangeRequest contains the one editable role.
type MemberChangeRequest struct {
	Role string `json:"role"`
}

// updateMember changes a member's role while retaining at least one Admin.
// @Summary Change workspace member role
// @Tags workspaces
// @Security BearerAuth
// @Param workspaceID path string true "Workspace ID"
// @Param userID path string true "User ID"
// @Param body body MemberChangeRequest true "New role"
// @Success 204
// @Failure 400,401,403,404,409,503 {object} ErrorResponse
// @Router /workspaces/{workspaceID}/members/{userID} [patch]
func (s *Server) updateMember(c *echo.Context) error { return s.members(c) }

// removeMember revokes an existing membership.
// @Summary Revoke workspace membership
// @Tags workspaces
// @Security BearerAuth
// @Param workspaceID path string true "Workspace ID"
// @Param userID path string true "User ID"
// @Success 204
// @Failure 401,403,404,409,503 {object} ErrorResponse
// @Router /workspaces/{workspaceID}/members/{userID} [delete]
func (s *Server) removeMember(c *echo.Context) error { return s.members(c) }

func publicAuth(method, path string) bool {
	return method == http.MethodGet && (path == "/v1/auth/google/start" || path == "/v1/auth/google/callback") || method == http.MethodPost && (path == "/v1/auth/refresh" || path == "/v1/auth/logout")
}
func unscopedAuth(path string) bool {
	return path == "/v1/me" || path == "/v1/workspaces" || path == "/v1/auth/logout-all"
}
func actionFor(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/v1/workspaces/") {
		return "members"
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return "read"
	}
	return "write"
}
