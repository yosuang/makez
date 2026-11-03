package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/artifact"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
	"golang.org/x/oauth2"
)

// These fixtures support the retained frontend browser journey. They contain no
// standalone backend test cases and preserve its PostgreSQL and mock OIDC setup.
type apiFixture struct {
	databaseURL string
	artifactDir string
	db          *store.Store
	files       *artifact.Store
	handler     http.Handler
	workspace   string
	token       string
}

func apiTestServer(t *testing.T) *apiFixture {
	t.Helper()
	databaseURL := os.Getenv("MAKEZ_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MAKEZ_TEST_DATABASE_URL for HTTP/PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [12]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	schema := "makez_api_test_" + hex.EncodeToString(nonce[:])
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
		t.Fatal(err)
	}
	// Isolate Outbox rows from a concurrently running development dispatcher.
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		if err := admin.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	db, err := store.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("PostgreSQL integration fixture ready")
	artifactDir := t.TempDir()
	files, err := artifact.New(artifactDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	workspace := "api-test-" + hex.EncodeToString(nonce[:])
	s := &Server{DB: db, Artifacts: files, Keys: map[string]string{"test-machine-token-a": workspace, "test-machine-token-b": workspace + "-other"}, Agents: map[string]core.AgentAdapter{"demo@1": agent.NewDemo()}}
	if err = db.RegisterMachines(ctx, s.Keys); err != nil {
		t.Fatal(err)
	}
	return &apiFixture{artifactDir: artifactDir, databaseURL: u.String(), db: db, files: files, handler: s.Handler(), workspace: workspace, token: "test-machine-token-a"}
}

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
