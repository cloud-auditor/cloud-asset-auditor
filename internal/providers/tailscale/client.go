package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultBaseURL is the Tailscale cloud control plane. Self-hosted /
// Headscale-compatible control planes override this via
// TAILSCALE_API_BASE_URL / --tailscale-api-url.
const defaultBaseURL = "https://api.tailscale.com"

// client is a thin Tailscale v2 API client. Same rationale as the NetBird
// client: the official tailscale.com module drags in the whole tsnet/wgengine
// stack (and cgo-adjacent platform code) for the sake of a handful of GETs,
// so the read surface we need is hand-rolled against net/http.
type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func newClient(baseURL, token string) *client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// apiError carries a non-2xx response. Tailscale's error envelope is
// {"message": "..."}; we keep the status + message and never the token,
// which only ever rides in a header.
type apiError struct {
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("tailscale API %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("tailscale API %d", e.StatusCode)
}

// getJSON performs an authenticated GET against path and decodes the JSON
// body into out. Tailscale API access tokens use the Bearer scheme (unlike
// NetBird's "Token" scheme) — an OAuth client secret works here too, since
// the API accepts both on the same header.
//
// The token is scrubbed from any returned error (invariant 4: never log
// secrets) as defense in depth; net/http never echoes a header value into a
// transport error, but a future change that puts the token near a URL must
// not be able to leak it through a wrapped error.
func (c *client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return c.redact(fmt.Errorf("tailscale: build request %s: %w", path, err))
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return c.redact(fmt.Errorf("tailscale: GET %s: %w", path, err))
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return parseAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return c.redact(fmt.Errorf("tailscale: decode %s: %w", path, err))
	}
	return nil
}

// redact strips the token from an error message.
func (c *client) redact(err error) error {
	if err == nil || c.token == "" {
		return err
	}
	if msg := err.Error(); strings.Contains(msg, c.token) {
		return errors.New(strings.ReplaceAll(msg, c.token, "***"))
	}
	return err
}

func parseAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var env struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &env)
	return &apiError{StatusCode: resp.StatusCode, Message: env.Message}
}

// isAuthError reports whether err is a 401/403 — used by Validate to give a
// crisp "bad/insufficient token" message.
func isAuthError(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden)
}

// isNotFound reports whether err is a 404. Several tailnet sub-resources
// (ACL grants on a free plan, DNS split-config) answer 404 rather than an
// empty document, which is a "not configured" signal, not a failure.
func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound
}
