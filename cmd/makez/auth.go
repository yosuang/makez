package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/makez-dev/makez/internal/api"
	"github.com/makez-dev/makez/internal/store"
)

func configureAuth(ctx context.Context, development bool) (*api.GoogleAuth, []string, store.SessionPolicy, error) {
	var origins []string
	for _, raw := range strings.Split(os.Getenv("MAKEZ_WEB_ORIGINS"), ",") {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		local := err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")
		if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (!development || !local || u.Scheme != "http")) {
			return nil, nil, store.SessionPolicy{}, errors.New("MAKEZ_WEB_ORIGINS requires exact HTTPS origins (loopback HTTP only in development)")
		}
		origins = append(origins, raw)
	}
	p := store.DefaultSessionPolicy()
	for name, target := range map[string]*time.Duration{"MAKEZ_ACCESS_TTL": &p.Access, "MAKEZ_REFRESH_TTL": &p.Refresh, "MAKEZ_SESSION_TTL": &p.Absolute, "MAKEZ_SESSION_IDLE_TTL": &p.Idle} {
		if raw := os.Getenv(name); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return nil, nil, p, errors.New("invalid session duration configuration")
			}
			*target = d
		}
	}
	if err := p.Validate(); err != nil {
		return nil, nil, p, err
	}
	id, secret, callback := os.Getenv("MAKEZ_GOOGLE_CLIENT_ID"), os.Getenv("MAKEZ_GOOGLE_CLIENT_SECRET"), os.Getenv("MAKEZ_GOOGLE_REDIRECT_URL")
	if id == "" && secret == "" && callback == "" {
		return nil, origins, p, nil
	}
	if len(origins) != 1 {
		return nil, nil, p, errors.New("google browser login requires exactly one MAKEZ_WEB_ORIGINS origin")
	}
	providerCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	g, err := api.NewGoogleAuth(providerCtx, api.GoogleConfig{ClientID: id, ClientSecret: secret, RedirectURL: callback, FrontendOrigin: origins[0], Development: development})
	return g, origins, p, err
}
