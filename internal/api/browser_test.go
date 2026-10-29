package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/makez-dev/makez/internal/agent"
	"github.com/makez-dev/makez/internal/core"
	"github.com/makez-dev/makez/internal/store"
)

type browserWorkspace struct{}

func (browserWorkspace) Read(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("verified browser artifact\n")), nil
}

// TestBrowserSessionJourney runs the real Web app against PostgreSQL and mock OIDC.
// CI enables it explicitly after installing Chromium; normal Go checks report the skip.
func TestBrowserSessionJourney(t *testing.T) {
	if os.Getenv("MAKEZ_BROWSER_TEST") != "1" {
		t.Skip("set MAKEZ_BROWSER_TEST=1 for the Chromium user journey")
	}
	f := apiTestServer(t)
	mock := newMockOIDC(t)
	user, err := f.db.LoginIdentity(t.Context(), store.Identity{Issuer: "https://accounts.google.com", Subject: "google-subject", Name: "Test User", Email: "test@example.com", EmailVerified: true})
	if err != nil {
		t.Fatal(err)
	}
	p := store.Principal{Kind: "user", ID: user.ID}
	workspaces, err := f.db.Workspaces(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	ws := workspaces[0].ID
	secondUser, err := f.db.LoginIdentity(t.Context(), store.Identity{Issuer: "https://accounts.google.com", Subject: "second-admin", Name: "Second Admin"})
	if err != nil {
		t.Fatal(err)
	}
	secondSpaces, err := f.db.Workspaces(t.Context(), store.Principal{Kind: "user", ID: secondUser.ID})
	if err != nil {
		t.Fatal(err)
	}
	fixtureDB, err := pgx.Connect(t.Context(), f.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixtureDB.Close(context.Background()) })
	if _, err = fixtureDB.Exec(t.Context(), `INSERT INTO makez_memberships(workspace_id,user_id,role) VALUES($1,$2,'Admin'),($3,$4,'Viewer')`, ws, secondUser.ID, secondSpaces[0].ID, user.ID); err != nil {
		t.Fatal(err)
	}
	adminPair, err := f.db.CreateSession(t.Context(), secondUser.ID, store.DefaultSessionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithPrincipal(t.Context(), p)
	task, _, err := f.db.Submit(ctx, ws, "browser-artifact", store.SubmitInput{Prompt: "Completed artifact task", AgentID: "demo", AgentVersion: "1", ProfileID: "default", ProfileVersion: "1", TimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	task, err = f.db.InitializeRun(ctx, ws, task.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.RunState{core.RunStopping, core.RunFinalizing} {
		if err = f.db.UpdateRun(ctx, task.Run.ID, store.RunUpdate{State: new(state)}); err != nil {
			t.Fatal(err)
		}
	}
	archived, err := f.files.Archive(ctx, task.Run.ID, browserWorkspace{}, []core.ArtifactCandidate{{Path: "result.txt", Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	a := archived[0]
	if err = f.db.Publish(ctx, ws, task.ID, core.TaskSucceeded, "", true, core.CleanupDone, []store.Artifact{{Path: "result.txt", StorageKey: a.StorageKey, SHA256: a.SHA256, Size: a.Size, MediaType: "text/plain"}}); err != nil {
		t.Fatal(err)
	}
	s := &Server{DB: f.db, Artifacts: f.files, Development: true, AllowedOrigins: []string{"http://127.0.0.1:5173"}, Agents: map[string]core.AgentAdapter{"demo@1": agent.NewDemo()}}
	server := httptest.NewServer(s.Handler())
	t.Cleanup(server.Close)
	g, err := newGoogleAuth(ctx, GoogleConfig{ClientID: "test-client", ClientSecret: "test-secret", RedirectURL: server.URL + "/v1/auth/google/callback", FrontendOrigin: s.AllowedOrigins[0], Development: true}, mock.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Google = g
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	webCtx, stop := context.WithCancel(t.Context())
	defer stop()
	web := exec.CommandContext(webCtx, "pnpm", "--dir", "apps/web", "dev", "--port", "5173")
	web.Dir = root
	web.Env = append(os.Environ(), "VITE_MAKEZ_SERVER_URL="+server.URL)
	var webLog bytes.Buffer
	web.Stdout = &webLog
	web.Stderr = &webLog
	if err = web.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(); _ = web.Wait() })
	ready := false
	for range 100 {
		response, e := http.Get("http://127.0.0.1:5173")
		if e == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		select {
		case <-t.Context().Done():
			t.Fatal("browser fixture canceled")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if !ready {
		stop()
		_ = web.Wait()
		t.Fatal("Web fixture did not start", webLog.String())
	}
	node := exec.CommandContext(t.Context(), "node", "tests/session.mjs")
	node.Dir = filepath.Join(root, "apps/web")
	node.Env = append(os.Environ(), "MAKEZ_BROWSER_URL=http://127.0.0.1:5173", "MAKEZ_BROWSER_API="+server.URL, "MAKEZ_BROWSER_ADMIN_ACCESS="+adminPair.Access, "MAKEZ_BROWSER_USER="+user.ID, "MAKEZ_BROWSER_WORKSPACE="+ws, "MAKEZ_BROWSER_VIEWER_WORKSPACE="+secondSpaces[0].ID)
	output, err := node.CombinedOutput()
	if err != nil {
		t.Fatalf("browser journey: %v\n%s", err, output)
	}
	t.Log(string(output))
}
