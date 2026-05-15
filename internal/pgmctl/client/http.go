package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	cfg "github.com/f1bonacc1/pgman-proxy/internal/pgmctl/config"
)

// Client is the pgmctl HTTP client. One client per invocation; FR-006
// forbids opening connections to more than one pgman-proxy peer in a
// single invocation.
type Client struct {
	base       *url.URL
	httpClient *http.Client
	tokenSrc   TokenSource
	userAgent  string

	// expectedCluster is validated against the server's announced
	// cluster id before any cluster-affecting operation (FR-010).
	expectedCluster string
}

// Options controls Client construction.
type Options struct {
	Resolved  *cfg.Resolved
	UserAgent string

	// SkipTLSVerify implements --insecure-skip-tls-verify. The
	// caller is responsible for emitting the loud yellow warning
	// on stderr; this flag is the wire-level toggle only.
	SkipTLSVerify bool

	// RequestTimeout is the overall per-request timeout. Defaults
	// to 30s if zero — global --timeout governs the command, but
	// the HTTP client needs a long-enough ceiling for SSE-style
	// long-lived requests.
	RequestTimeout time.Duration
}

// New constructs a Client from a resolved profile.
func New(opts Options) (*Client, error) {
	if opts.Resolved == nil {
		return nil, errors.New("client.New: nil Resolved")
	}
	base, err := url.Parse(opts.Resolved.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint %q: %w", opts.Resolved.Endpoint, err)
	}
	if base.Scheme != "https" && !isLoopbackHost(base.Host) {
		return nil, fmt.Errorf("endpoint %q must use https:// unless host is loopback", opts.Resolved.Endpoint)
	}

	tlsCfg, err := buildTLSConfig(opts.Resolved.TLS, opts.SkipTLSVerify)
	if err != nil {
		return nil, err
	}

	tokenSrc, err := NewTokenSource(opts.Resolved)
	if err != nil {
		return nil, err
	}

	transport := &http.Transport{
		TLSClientConfig:   tlsCfg,
		ForceAttemptHTTP2: true,
	}

	timeout := opts.RequestTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "pgmctl/dev"
	}

	return &Client{
		base:            base,
		httpClient:      &http.Client{Transport: transport, Timeout: timeout},
		tokenSrc:        tokenSrc,
		userAgent:       ua,
		expectedCluster: opts.Resolved.ExpectedCluster,
	}, nil
}

// Endpoint returns the configured base URL string (for diagnostics).
func (c *Client) Endpoint() string { return c.base.String() }

// TokenSourceID returns the non-secret label identifying where the
// bearer comes from (for --verbose output).
func (c *Client) TokenSourceID() string { return c.tokenSrc.SourceID() }

// ExpectedCluster returns the cluster name pinned by the active
// context or the --cluster flag (or "" if none).
func (c *Client) ExpectedCluster() string { return c.expectedCluster }

// Envelope is the LCM response envelope from contracts/lcm.md. The
// `engine_result` field is left as raw JSON so each subcommand can
// unmarshal into its own typed struct.
type Envelope struct {
	Operation    string          `json:"operation"`
	RequestID    string          `json:"request_id"`
	Outcome      string          `json:"outcome"`
	EngineResult json.RawMessage `json:"engine_result,omitempty"`
	Error        *ErrorPayload   `json:"error,omitempty"`
}

// ErrorPayload is the LCM `error` block.
type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError wraps a non-accepted response so callers can inspect the
// code without parsing again.
type APIError struct {
	HTTPStatus int
	Code       string
	Message    string
	Operation  string
	RequestID  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("pgman-proxy %s: %s (HTTP %d, request_id=%s)", e.Code, e.Message, e.HTTPStatus, e.RequestID)
}

// GetJSON does a GET that expects a 200-OK LCM envelope.
func (c *Client) GetJSON(ctx context.Context, path string) (*Envelope, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// PostJSON does a POST with a JSON body and expects a 200-OK LCM
// envelope. Per FR-039, mutating ops MUST NEVER be retried; this
// method does not retry. Caller MUST ensure body has the
// `request_id` field set when it carries one.
func (c *Client) PostJSON(ctx context.Context, path string, body any) (*Envelope, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
	}
	return c.do(ctx, http.MethodPost, path, &buf)
}

// NewRequestID returns a fresh ULID for the X-Request-Id header /
// audit correlation. FR-039: never reuse on retry.
func NewRequestID() string {
	return strings.ToLower(ulid.Make().String())
}

// StreamSSE issues a long-lived GET that expects a `text/event-stream`
// response and returns the live HTTP response so the caller can parse
// SSE frames. The caller MUST `defer resp.Body.Close()`.
//
// Distinct from GetJSON because:
//   - The 30s default per-request timeout would kill the stream; this
//     method bypasses it (the context governs cancellation).
//   - Auth + UA + cluster-pin remain identical to the JSON path.
//   - Extra headers (Last-Event-ID / etc.) come through via the
//     headers map.
func (c *Client) StreamSSE(ctx context.Context, path string, headers map[string]string) (*http.Response, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse path %q: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	tok, err := c.tokenSrc.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve bearer token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("X-Request-Id", NewRequestID())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// Use a fresh client with no timeout — SSE streams live for the
	// lifetime of ctx. We borrow the transport so TLS / CA configuration
	// is identical.
	cli := &http.Client{Transport: c.httpClient.Transport}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, &NetworkError{Cause: err, Endpoint: u.String()}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		resp.Body.Close()
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: "auth_invalid", Message: "authentication failed"}
	}
	if resp.StatusCode == http.StatusNotAcceptable {
		resp.Body.Close()
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: "not_acceptable", Message: "server refused SSE — check that the endpoint supports /v1/watch/*"}
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: "stream_open_failed", Message: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*Envelope, error) {
	u, err := c.base.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse path %q: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	tok, err := c.tokenSrc.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve bearer token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Request-Id", NewRequestID())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Network / TLS / handshake failure — distinct from "cluster
		// unhealthy" so the caller can map to EX_NETWORK (65).
		return nil, &NetworkError{Cause: err, Endpoint: u.String()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Try to extract the envelope but fall back to a synthetic
		// error if the body isn't an envelope.
		var env Envelope
		_ = json.Unmarshal(raw, &env)
		msg := "authentication failed"
		code := "auth_invalid"
		if env.Error != nil {
			msg = env.Error.Message
			code = env.Error.Code
		}
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: code, Message: msg, Operation: env.Operation, RequestID: env.RequestID}
	}

	if len(raw) == 0 {
		return nil, fmt.Errorf("empty response from %s (HTTP %d)", u.String(), resp.StatusCode)
	}

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode envelope (HTTP %d): %w", resp.StatusCode, err)
	}
	if env.Outcome != "accepted" {
		ap := &APIError{HTTPStatus: resp.StatusCode, Operation: env.Operation, RequestID: env.RequestID}
		if env.Error != nil {
			ap.Code = env.Error.Code
			ap.Message = env.Error.Message
		} else {
			ap.Code = "non_accepted_outcome"
			ap.Message = "outcome=" + env.Outcome
		}
		return &env, ap
	}
	return &env, nil
}

// NetworkError signals a transport-level failure (connect refused,
// TLS handshake error, DNS, etc.). Distinct from APIError so the
// caller can map to EX_NETWORK (65) per FR-037.
type NetworkError struct {
	Cause    error
	Endpoint string
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("connect %s: %v", e.Endpoint, e.Cause)
}

func (e *NetworkError) Unwrap() error { return e.Cause }

func buildTLSConfig(b cfg.TLSBlock, skip bool) (*tls.Config, error) {
	t := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if skip || b.InsecureSkipTLSVerify {
		t.InsecureSkipVerify = true
	}
	if b.ServerName != "" {
		t.ServerName = b.ServerName
	}
	if b.CAFile != "" {
		raw, err := os.ReadFile(b.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls.ca_file %s: %w", b.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(raw) {
			return nil, fmt.Errorf("tls.ca_file %s contained no parseable PEM certificates", b.CAFile)
		}
		t.RootCAs = pool
	}
	return t, nil
}

func isLoopbackHost(host string) bool {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return strings.HasPrefix(host, "127.")
}
