package netbird

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

// defaultBaseURL is the NetBird cloud Management API. Self-hosted instances
// override this via NETBIRD_MANAGEMENT_URL / --netbird-management-url.
const defaultBaseURL = "https://api.netbird.io"

// client is a thin NetBird Management API client. NetBird's public API is
// plain REST with Personal-Access-Token auth, so we hand-roll the read surface
// we need rather than vendor the netbirdio/netbird module — that module pulls
// the entire management server + gRPC stack and would balloon the static
// binary (and its dependency graph) for the sake of a handful of GETs.
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

// apiError carries a non-2xx response. NetBird's error envelope is
// {"message": "...", "code": N}; we keep the status + message (never the
// request token, which only ever rides in a header).
type apiError struct {
	StatusCode int
	Message    string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("netbird API %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("netbird API %d", e.StatusCode)
}

// getJSON performs an authenticated GET against path (e.g. "/api/peers") and
// decodes the JSON body into out. The token rides in the Authorization header
// and is scrubbed from any returned error as a defense-in-depth measure
// (invariant 4: never log secrets), even though net/http never echoes a header
// value into transport errors.
func (c *client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return c.redact(fmt.Errorf("netbird: build request %s: %w", path, err))
	}
	// NetBird Personal Access Tokens authenticate with the "Token" scheme
	// (not "Bearer", which is reserved for OAuth bearer JWTs).
	req.Header.Set("Authorization", "Token "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return c.redact(fmt.Errorf("netbird: GET %s: %w", path, err))
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
		return c.redact(fmt.Errorf("netbird: decode %s: %w", path, err))
	}
	return nil
}

// redact strips the token from an error message. The token is header-only so
// this is belt-and-suspenders, but it guarantees a future change that puts the
// token anywhere near a URL can't leak it through a wrapped error.
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
