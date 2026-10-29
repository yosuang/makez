package api

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"
)

type mockGrant struct{ nonce, challenge string }
type mockOIDC struct {
	server       *httptest.Server
	mu           sync.Mutex
	grants       map[string]mockGrant
	change       func(map[string]any)
	exchanges    int
	badSignature bool
	badPKCE      bool
}

func newMockOIDC(t *testing.T) *mockOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"))
	if err != nil {
		t.Fatal(err)
	}
	m := &mockOIDC{grants: map[string]mockGrant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeMockJSON(w, map[string]any{"issuer": m.server.URL, "authorization_endpoint": m.server.URL + "/authorize", "token_endpoint": m.server.URL + "/token", "jwks_uri": m.server.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		writeMockJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		code := rand.Text()
		m.mu.Lock()
		challenge := q.Get("code_challenge")
		if m.badPKCE {
			challenge = "different-challenge"
		}
		m.grants[code] = mockGrant{nonce: q.Get("nonce"), challenge: challenge}
		m.mu.Unlock()
		callback, err := url.Parse(q.Get("redirect_uri"))
		if err != nil {
			http.Error(w, "redirect", http.StatusBadRequest)
			return
		}
		params := callback.Query()
		params.Set("state", q.Get("state"))
		params.Set("code", code)
		callback.RawQuery = params.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "form", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.exchanges++
		code := r.Form.Get("code")
		grant, ok := m.grants[code]
		delete(m.grants, code)
		if !ok || oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != grant.challenge {
			http.Error(w, "invalid grant", http.StatusBadRequest)
			return
		}
		claims := map[string]any{"iss": m.server.URL, "aud": "test-client", "sub": "google-subject", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": grant.nonce, "email": "test@example.com", "email_verified": true, "name": "Test User"}
		if m.change != nil {
			m.change(claims)
		}
		signed, err := jwt.Signed(signer).Claims(claims).Serialize()
		if err != nil {
			http.Error(w, "sign", http.StatusInternalServerError)
			return
		}
		if m.badSignature {
			parts := strings.Split(signed, ".")
			signature, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				panic(err)
			}
			signature[0] ^= 1
			parts[2] = base64.RawURLEncoding.EncodeToString(signature)
			signed = strings.Join(parts, ".")
		}
		writeMockJSON(w, map[string]any{"access_token": "mock-provider-token", "token_type": "Bearer", "expires_in": 3600, "id_token": signed})
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}
func writeMockJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "encoding", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(data)
}

func oidcFixture(t *testing.T) (*Server, http.Handler, *mockOIDC) {
	t.Helper()
	f := apiTestServer(t)
	mock := newMockOIDC(t)
	g, err := newGoogleAuth(t.Context(), GoogleConfig{ClientID: "test-client", ClientSecret: "isolated-test-secret", RedirectURL: "http://127.0.0.1:8088/v1/auth/google/callback", FrontendOrigin: "http://127.0.0.1:5173", Development: true}, mock.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: f.db, Google: g, Development: true, AllowedOrigins: []string{"http://127.0.0.1:5173"}, Keys: map[string]string{f.token: f.workspace}}
	return s, s.Handler(), mock
}

func loginRequest(t *testing.T, s *Server, h http.Handler) *http.Request {
	t.Helper()
	start := httptest.NewRecorder()
	h.ServeHTTP(start, httptest.NewRequest(http.MethodGet, "/v1/auth/google/start", nil))
	if start.Code != 302 {
		t.Fatalf("start=%d %s", start.Code, start.Body.String())
	}
	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Get(start.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck // Test response is read-only.
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, location.RequestURI(), nil)
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == s.cookieName("login") {
			request.AddCookie(cookie)
		}
	}
	return request
}

func loginCallback(t *testing.T, s *Server, h http.Handler, _ *mockOIDC) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	request := loginRequest(t, s, h)
	callback := httptest.NewRecorder()
	h.ServeHTTP(callback, request)
	return callback, request
}

func TestMockOIDCLoginAndCookieSession(t *testing.T) {
	s, h, m := oidcFixture(t)
	callback, request := loginCallback(t, s, h, m)
	if callback.Code != 303 || callback.Header().Get("Location") != "http://127.0.0.1:5173/" {
		t.Fatalf("callback=%d %s", callback.Code, callback.Body.String())
	}
	var refresh *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == s.cookieName("refresh") {
			refresh = cookie
		}
	}
	if refresh == nil || !refresh.HttpOnly || refresh.SameSite != http.SameSiteLaxMode || refresh.Path != "/" {
		t.Fatal("refresh cookie attributes")
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/refresh", nil)
	r.AddCookie(refresh)
	r.Header.Set("Origin", s.AllowedOrigins[0])
	r.Header.Set("X-Makez-CSRF", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("refresh=%d %s", w.Code, w.Body.String())
	}
	var pair AccessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &pair); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pair.AccessToken, "mza_") || strings.Contains(w.Body.String(), "mzr_") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("token response contract")
	}
	p, err := s.DB.AuthenticateAccess(t.Context(), pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	spaces, err := s.DB.Workspaces(t.Context(), p)
	if err != nil || len(spaces) != 1 || spaces[0].Role != "Admin" {
		t.Fatal("default workspace missing", err)
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, request)
	if replay.Code != 401 {
		t.Fatalf("callback replay=%d", replay.Code)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.exchanges != 1 {
		t.Fatal("replayed code exchanged")
	}
}

func TestMockOIDCRejectsInvalidClaims(t *testing.T) {
	for _, field := range []string{"iss", "aud", "nonce", "exp", "sub"} {
		t.Run(field, func(t *testing.T) {
			s, h, m := oidcFixture(t)
			m.change = func(claims map[string]any) {
				switch field {
				case "exp":
					claims[field] = time.Now().Add(-time.Hour).Unix()
				case "sub":
					claims[field] = ""
				default:
					claims[field] = "wrong"
				}
			}
			response, _ := loginCallback(t, s, h, m)
			if response.Code != 401 {
				t.Fatalf("invalid %s status=%d", field, response.Code)
			}
		})
	}
}

func TestAuthCSRFAndExactExceptions(t *testing.T) {
	s, h, _ := oidcFixture(t)
	for _, tc := range []struct {
		method, path, origin, csrf string
		status                     int
	}{
		{"POST", "/v1/auth/refresh", "", "", 403},
		{"POST", "/v1/auth/refresh", "https://evil.example", "1", 403},
		{"POST", "/v1/auth/logout", s.AllowedOrigins[0], "", 403},
		{"POST", "/v1/auth/logout", s.AllowedOrigins[0], "1", 204},
		{"POST", "/v1/auth/logout-all", s.AllowedOrigins[0], "1", 401},
		{"GET", "/v1/auth/private", s.AllowedOrigins[0], "", 401},
		{"POST", "/v1/tasks", s.AllowedOrigins[0], "1", 401},
		{"GET", "/v1/auth/google/start?return_to=https://evil.example", s.AllowedOrigins[0], "", 400},
	} {
		t.Run(tc.method+tc.path+tc.origin+tc.csrf, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-Makez-CSRF", tc.csrf)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d %s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("Access-Control-Allow-Origin") == "*" {
				t.Fatal("wildcard credentialed CORS")
			}
		})
	}
}

func TestUserTokenCannotFallbackToMachineKey(t *testing.T) {
	f := apiTestServer(t)
	token := "mza_" + strings.Repeat("a", 43)
	s := &Server{DB: f.db, Keys: map[string]string{token: f.workspace}}
	r := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("invalid human token fell back", w.Code)
	}
}

func TestSecureCookieProductionAttributes(t *testing.T) {
	s := &Server{}
	if s.cookieName("refresh") != "__Host-makez-refresh" {
		t.Fatal("production cookie prefix")
	}
}

func TestCrossSiteConfigurationRequiresExplicitDesign(t *testing.T) {
	_, err := newGoogleAuth(t.Context(), GoogleConfig{ClientID: "test", ClientSecret: "isolated", RedirectURL: "https://api.other.example/v1/auth/google/callback", FrontendOrigin: "https://console.example.com"}, "https://must-not-contact.example")
	if err == nil || !strings.Contains(err.Error(), "cross-site") {
		t.Fatalf("cross-site configuration accepted: %v", err)
	}
}

func TestMockOIDCRejectsSignatureAndPKCEMismatch(t *testing.T) {
	for _, kind := range []string{"signature", "pkce"} {
		t.Run(kind, func(t *testing.T) {
			s, h, m := oidcFixture(t)
			m.badSignature = kind == "signature"
			m.badPKCE = kind == "pkce"
			response, request := loginCallback(t, s, h, m)
			if response.Code != 401 {
				t.Fatalf("invalid %s accepted: %d", kind, response.Code)
			}
			retry := httptest.NewRecorder()
			h.ServeHTTP(retry, request)
			if retry.Code != 401 {
				t.Fatal("failed login transaction was reusable")
			}
		})
	}
}

func TestMockOIDCRejectsUnboundStateBeforeExchange(t *testing.T) {
	for _, kind := range []string{"state", "browser"} {
		t.Run(kind, func(t *testing.T) {
			s, h, m := oidcFixture(t)
			request := loginRequest(t, s, h)
			if kind == "state" {
				values := request.URL.Query()
				values.Set("state", strings.Repeat("a", 43))
				request.URL.RawQuery = values.Encode()
			} else {
				request.Header.Del("Cookie")
				request.AddCookie(&http.Cookie{Name: s.cookieName("login"), Value: strings.Repeat("a", 43)})
			}
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != 401 {
				t.Fatalf("unbound %s accepted: %d", kind, response.Code)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			if m.exchanges != 0 {
				t.Fatal("unbound code was exchanged")
			}
		})
	}
}
