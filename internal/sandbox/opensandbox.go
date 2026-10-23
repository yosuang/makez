// Package sandbox implements OpenSandbox's lifecycle API and makez's runtime protocol.
package sandbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/makez-dev/makez/internal/core"
)

// Config specifies the OpenSandbox connection and runtime settings.
type Config struct {
	BaseURL, APIKey, InstallationID, ControlKey, Image string
	HTTPClient                                         *http.Client
}

// Provider implements sandbox operations using OpenSandbox and the supervisor.
type Provider struct {
	cfg      Config
	client   *http.Client
	mu       sync.Mutex
	attempts map[string]string
}

// Error classifies a provider failure without exposing sensitive response data.
type Error struct {
	Kind    core.FailureCode
	Message string
}

func (e *Error) Error() string { return e.Message }

// Code returns the failure classification.
func (e *Error) Code() core.FailureCode                { return e.Kind }
func fail(code core.FailureCode, message string) error { return &Error{code, message} }

// New validates configuration and constructs an OpenSandbox provider.
func New(cfg Config) (*Provider, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fail(core.InvalidInput, "invalid OpenSandbox URL")
	}
	if cfg.InstallationID == "" || len(cfg.ControlKey) < 32 || cfg.Image == "" {
		return nil, fail(core.InvalidInput, "installation ID, image and a 32-byte control key are required")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if !strings.HasSuffix(cfg.BaseURL, "/v1") {
		cfg.BaseURL += "/v1"
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	clone := *c
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Provider{cfg: cfg, client: &clone, attempts: map[string]string{}}, nil
}

// BackendID returns the identity used to associate runs with this provider.
func (p *Provider) BackendID() string { return "opensandbox" }

// Capabilities reports the supported operations and runtime profile limits.
func (p *Provider) Capabilities(_ context.Context, id, version string) (core.SandboxCapabilities, error) {
	if id != "default" || version != "1" {
		return core.SandboxCapabilities{}, fail(core.Unsupported, "unknown runtime profile")
	}
	return core.SandboxCapabilities{Supported: []core.Capability{core.BackgroundProcess, core.OperationLookup, core.ReplayableOutput, core.ProcessStop, core.FileAccess, core.FiniteLifetime, core.SecretInjection}, MaxLifetime: 2 * time.Hour, Renewable: false}, nil
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:16])
}
func (p *Provider) labels(key core.OperationKey) map[string]string {
	return map[string]string{"makez-installation": digest(p.cfg.InstallationID), "makez-operation": digest(key), "makez-workspace": digest(key.WorkspaceID), "makez-task": digest(key.TaskID), "makez-run": digest(key.RunID)}
}
func (p *Provider) token(key core.OperationKey) string {
	m := hmac.New(sha256.New, []byte(p.cfg.ControlKey))
	b, _ := json.Marshal(key)
	m.Write(b)
	return hex.EncodeToString(m.Sum(nil))
}
func (p *Provider) owned(s resource, key core.OperationKey) bool {
	for k, v := range p.labels(key) {
		if s.Metadata[k] != v {
			return false
		}
	}
	return true
}

type resource struct {
	ID       string            `json:"id"`
	Metadata map[string]string `json:"metadata"`
	Status   struct {
		State string `json:"state"`
	} `json:"status"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (p *Provider) request(ctx context.Context, method, target string, body any, headers map[string]string, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		code := core.Transient
		if method != "GET" {
			code = core.OutcomeUnknown
		}
		return fail(code, "OpenSandbox request failed")
	}
	defer resp.Body.Close() //nolint:errcheck // Response decoding reports read failures; closing only releases transport resources.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := core.Transient
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			code = core.InvalidInput
		case http.StatusUnauthorized, http.StatusForbidden:
			code = core.PermissionDenied
		case http.StatusNotFound:
			code = core.ResourceLost
		case http.StatusConflict:
			code = core.Conflict
		default:
			if method != "GET" {
				code = core.OutcomeUnknown
			}
		}
		// Backend error bodies can contain command environments; do not persist them.
		return fail(code, fmt.Sprintf("OpenSandbox %s returned HTTP %d", method, resp.StatusCode))
	}
	if out != nil {
		if err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil {
			code := core.Transient
			if method != "GET" {
				code = core.OutcomeUnknown
			}
			return fail(code, "invalid OpenSandbox response")
		}
	}
	return nil
}
func (p *Provider) lifecycle(ctx context.Context, method, path string, in, out any) error {
	return p.request(ctx, method, p.cfg.BaseURL+path, in, map[string]string{"OPEN-SANDBOX-API-KEY": p.cfg.APIKey}, out)
}
func (p *Provider) list(ctx context.Context, key core.OperationKey) ([]resource, error) {
	filter := url.Values{}
	for k, v := range p.labels(key) {
		filter.Set(k, v)
	}
	var all []resource
	for page := 1; page <= 10000; page++ {
		q := url.Values{"metadata": {filter.Encode()}, "page": {strconv.Itoa(page)}, "pageSize": {"100"}}
		var response struct {
			Items      []resource `json:"items"`
			Pagination struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pagination"`
		}
		if err := p.lifecycle(ctx, "GET", "/sandboxes?"+q.Encode(), nil, &response); err != nil {
			return nil, err
		}
		for _, r := range response.Items {
			if p.owned(r, key) {
				all = append(all, r)
			}
		}
		if !response.Pagination.HasNextPage {
			return all, nil
		}
	}
	return nil, fail(core.OutcomeUnknown, "sandbox enumeration exceeded page bound")
}

// Inspect finds sandbox resources owned by an operation key.
func (p *Provider) Inspect(ctx context.Context, key core.OperationKey) (core.SandboxObservation, error) {
	rows, err := p.list(ctx, key)
	o := core.SandboxObservation{State: core.SandboxAbsent, ObservedAt: time.Now().UTC()}
	if err != nil {
		return o, err
	}
	for _, r := range rows {
		o.Matches = append(o.Matches, core.SandboxRef{BackendID: p.BackendID(), ID: r.ID, Key: key})
	}
	if len(rows) > 1 {
		o.State = core.SandboxUnknown
		return o, nil
	}
	if len(rows) == 0 {
		return o, nil
	}
	o.ExpiresAt = rows[0].ExpiresAt
	switch rows[0].Status.State {
	case "Running":
		o.State = core.SandboxReady
	case "Pending", "Creating", "Starting":
		o.State = core.SandboxCreating
	case "Stopping":
		o.State = core.SandboxDeleting
	default:
		o.State = core.SandboxUnknown
	}
	return o, nil
}

// Ensure reconciles creation of the sandbox for an operation key.
func (p *Provider) Ensure(ctx context.Context, r core.SandboxRequest) (core.SandboxRef, error) {
	empty := core.SandboxRef{}
	if _, err := p.Capabilities(ctx, r.RuntimeProfileID, r.RuntimeProfileVersion); err != nil {
		return empty, err
	}
	ttl := time.Until(r.ExpiresAt)
	if ttl < time.Minute || ttl > 2*time.Hour {
		return empty, fail(core.InvalidInput, "sandbox lifetime must be between one minute and two hours")
	}
	if r.NetworkPolicyRef != "" || r.Limits.DiskBytes != 0 {
		return empty, fail(core.Unsupported, "network policy and disk limits are not implemented by this runtime profile")
	}
	hash := digest(r)
	key := digest(r.Key)
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, err := p.list(ctx, r.Key)
	if err != nil {
		return empty, err
	}
	if len(rows) > 1 {
		return empty, fail(core.Conflict, "multiple sandboxes share this operation")
	}
	if len(rows) == 1 {
		if rows[0].Metadata["makez-request"] != hash {
			return empty, fail(core.Conflict, "sandbox operation request differs")
		}
		ref := core.SandboxRef{BackendID: p.BackendID(), ID: rows[0].ID, Key: r.Key}
		return ref, p.waitReady(ctx, ref)
	}
	if previous, ok := p.attempts[key]; ok {
		if previous != hash {
			return empty, fail(core.Conflict, "sandbox operation request differs")
		}
		return empty, fail(core.OutcomeUnknown, "create was already attempted; reconcile its outcome")
	}
	// The durable caller must also fence this operation across Worker restarts.
	p.attempts[key] = hash
	labels := p.labels(r.Key)
	labels["makez-request"] = hash
	cpu := r.Limits.MilliCPU
	if cpu == 0 {
		cpu = 1000
	}
	memory := r.Limits.MemoryBytes
	if memory == 0 {
		memory = 1 << 30
	}
	payload := map[string]any{"image": map[string]string{"uri": p.cfg.Image}, "timeout": int64(ttl.Seconds()), "resourceLimits": map[string]string{"cpu": fmt.Sprintf("%dm", cpu), "memory": strconv.FormatInt(memory, 10)}, "metadata": labels, "env": map[string]string{"MAKEZ_CONTROL_TOKEN": p.token(r.Key)}, "entrypoint": []string{"/usr/local/bin/sandboxd"}}
	var created resource
	if err = p.lifecycle(ctx, "POST", "/sandboxes", payload, &created); err != nil {
		return empty, err
	}
	if created.ID == "" {
		return empty, fail(core.OutcomeUnknown, "create response has no sandbox ID")
	}
	ref := core.SandboxRef{BackendID: p.BackendID(), ID: created.ID, Key: r.Key}
	return ref, p.waitReady(ctx, ref)
}
func (p *Provider) waitReady(ctx context.Context, ref core.SandboxRef) error {
	readyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	key := ref.Key
	key.Step = "agent"
	for {
		_, err := p.Process(readyCtx, ref, key)
		if err == nil {
			return nil
		}
		if pe, ok := errors.AsType[core.ProviderError](err); !ok || (pe.Code() != core.Transient && pe.Code() != core.OutcomeUnknown) {
			return err
		}
		select {
		case <-readyCtx.Done():
			return fail(core.Transient, "sandbox created but runtime readiness was not confirmed")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Renew reports that this provider's fixed lifetime profile does not support renewal.
func (p *Provider) Renew(context.Context, core.SandboxRef, time.Time) error {
	return fail(core.Unsupported, "fixed lifetime runtime profile does not renew")
}

// Delete removes sandbox resources owned by an operation key.
func (p *Provider) Delete(ctx context.Context, key core.OperationKey) error {
	rows, err := p.list(ctx, key)
	if err != nil {
		return err
	}
	for _, r := range rows {
		err = p.lifecycle(ctx, "DELETE", "/sandboxes/"+url.PathEscape(r.ID), nil, nil)
		pe, resourceError := errors.AsType[core.ProviderError](err)
		if err != nil && (!resourceError || pe.Code() != core.ResourceLost) {
			return err
		}
	}
	for {
		rows, err = p.list(ctx, key)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fail(core.OutcomeUnknown, "sandbox deletion not confirmed")
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (p *Provider) runtime(ctx context.Context, s core.SandboxRef, path string, in, out any) error {
	if s.BackendID != p.BackendID() {
		return fail(core.InvalidInput, "incorrect backend")
	}
	var r resource
	if err := p.lifecycle(ctx, "GET", "/sandboxes/"+url.PathEscape(s.ID), nil, &r); err != nil {
		return err
	}
	if !p.owned(r, s.Key) {
		return fail(core.PermissionDenied, "sandbox ownership does not match")
	}
	var ep struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	if err := p.lifecycle(ctx, "GET", "/sandboxes/"+url.PathEscape(s.ID)+"/endpoints/8099?use_server_proxy=true", nil, &ep); err != nil {
		return err
	}
	target := ep.Endpoint
	if !strings.Contains(target, "://") {
		u, _ := url.Parse(p.cfg.BaseURL)
		target = u.Scheme + "://" + target
	}
	if payload, ok := in.(startPayload); ok && len(payload.SecretValues) > 0 && !secretTransportAllowed(target) {
		return fail(core.PermissionDenied, "secret transport requires HTTPS or a loopback endpoint")
	}
	if ep.Headers == nil {
		ep.Headers = map[string]string{}
	}
	ep.Headers["X-Makez-Control-Token"] = p.token(s.Key)
	var envelope struct {
		Error string           `json:"error"`
		Code  core.FailureCode `json:"code"`
		Data  json.RawMessage  `json:"data"`
	}
	if err := p.request(ctx, "POST", strings.TrimRight(target, "/")+path, in, ep.Headers, &envelope); err != nil {
		if pe, ok := errors.AsType[core.ProviderError](err); ok && (path == "/process" || path == "/output" || path == "/read") && pe.Code() == core.OutcomeUnknown {
			return fail(core.Transient, pe.Error())
		}
		return err
	}
	if envelope.Error != "" {
		return fail(envelope.Code, envelope.Error)
	}
	if out != nil {
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fail(core.OutcomeUnknown, "invalid runtime response")
		}
	}
	return nil
}

type startPayload struct {
	core.StartRequest
	SecretValues map[string]string
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z_0-9]*$`)

func secretTransportAllowed(target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	if u.Hostname() == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(u.Hostname())
	return err == nil && address.IsLoopback()
}

func validateSecrets(r core.StartRequest) error {
	if len(r.SecretRefs) != len(r.SecretValues) || len(r.SecretRefs) > 32 {
		return fail(core.InvalidInput, "secret references require exactly matching resolved values")
	}
	total := 0
	for name, ref := range r.SecretRefs {
		value, exists := r.SecretValues[name]
		_, collision := r.Command.Env[name]
		if !exists || ref == "" || !envName.MatchString(name) || strings.HasPrefix(name, "MAKEZ_") || collision || value == "" || !utf8.ValidString(value) || strings.ContainsRune(value, 0) || strings.Contains("[REDACTED]", value) || len(value) > 65536 {
			return fail(core.InvalidInput, "invalid secret injection mapping")
		}
		total += len(value)
	}
	if total > 128<<10 {
		return fail(core.InvalidInput, "secret injection exceeds size limit")
	}
	return nil
}

// Start launches or reconciles a keyed process through the supervisor.
func (p *Provider) Start(ctx context.Context, s core.SandboxRef, r core.StartRequest) (core.ProcessRef, error) {
	if err := validateSecrets(r); err != nil {
		return core.ProcessRef{}, err
	}
	if len(r.SecretValues) > 0 && !secretTransportAllowed(p.cfg.BaseURL) {
		return core.ProcessRef{}, fail(core.PermissionDenied, "secret transport requires HTTPS or a loopback endpoint")
	}
	var o core.ProcessObservation
	err := p.runtime(ctx, s, "/start", startPayload{StartRequest: r, SecretValues: r.SecretValues}, &o)
	if err != nil {
		return core.ProcessRef{}, err
	}
	if o.Ref == nil {
		return core.ProcessRef{}, fail(core.OutcomeUnknown, "runtime has no confirmed process receipt")
	}
	o.Ref.Sandbox = s
	return *o.Ref, nil
}

// Process retrieves the supervisor’s receipt for a keyed execution.
func (p *Provider) Process(ctx context.Context, s core.SandboxRef, key core.OperationKey) (core.ProcessObservation, error) {
	var o core.ProcessObservation
	err := p.runtime(ctx, s, "/process", map[string]any{"Key": key}, &o)
	if o.Ref != nil {
		o.Ref.Sandbox = s
	}
	return o, err
}

// Output reads a page of replayable process output.
func (p *Provider) Output(ctx context.Context, r core.ProcessRef, cursor string, maxBytes int) (core.OutputPage, error) {
	var o core.OutputPage
	err := p.runtime(ctx, r.Sandbox, "/output", map[string]any{"Key": r.Key, "Cursor": cursor, "MaxBytes": maxBytes}, &o)
	return o, err
}

// Stop requests termination of a keyed execution through the supervisor.
func (p *Provider) Stop(ctx context.Context, s core.SandboxRef, key core.OperationKey, policy core.StopPolicy) error {
	return p.runtime(ctx, s, "/stop", map[string]any{"Key": key, "GraceSeconds": policy.GracePeriod.Seconds(), "Signal": policy.Signal}, nil)
}

// ReadFile opens a workspace file through the supervisor.
func (p *Provider) ReadFile(ctx context.Context, s core.SandboxRef, path string) (io.ReadCloser, error) {
	var o struct{ Data []byte }
	if err := p.runtime(ctx, s, "/read", map[string]string{"Path": path}, &o); err != nil {
		if pe, ok := errors.AsType[core.ProviderError](err); ok && pe.Code() == core.ResourceLost && pe.Error() == "workspace file does not exist" {
			return nil, fmt.Errorf("workspace file missing: %w", fs.ErrNotExist)
		}
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(o.Data)), nil
}

// WriteFile writes a bounded input file through the supervisor.
func (p *Provider) WriteFile(ctx context.Context, s core.SandboxRef, path string, contents io.Reader, size int64) error {
	if size < 0 || size > 16<<20 {
		return fail(core.InvalidInput, "input file exceeds 16 MiB")
	}
	b, err := io.ReadAll(io.LimitReader(contents, size+1))
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fail(core.InvalidInput, "input file size differs")
	}
	return p.runtime(ctx, s, "/write", map[string]any{"Path": path, "Data": b}, nil)
}

var _ core.SandboxProvider = (*Provider)(nil)
