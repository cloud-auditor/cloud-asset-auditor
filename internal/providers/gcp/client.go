package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	defaultBaseURL = "https://cloudasset.googleapis.com"
	// cloud-platform is the scope the Cloud Asset Inventory API documents;
	// actual read-only-ness is enforced by IAM (roles/cloudasset.viewer).
	assetScope = "https://www.googleapis.com/auth/cloud-platform"
	pageSize   = "500" // the API cap
)

// client is a thin Cloud Asset Inventory REST client. The authenticated
// http.Client is resolved once from Application Default Credentials (the same
// chain gcloud and the SDKs use: GOOGLE_APPLICATION_CREDENTIALS service-account
// key → gcloud user creds → GCE/GKE metadata / workload identity).
type client struct {
	baseURL string

	once    sync.Once
	hc      *http.Client // authed client; a pre-set value (tests) skips ADC
	credErr error
}

func newClient(baseURL string) *client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &client{baseURL: baseURL}
}

// authedClient resolves ADC exactly once and returns an http.Client that
// injects and refreshes the bearer token. A pre-set c.hc (test injection)
// short-circuits ADC.
func (c *client) authedClient(ctx context.Context) (*http.Client, error) {
	c.once.Do(func() {
		if c.hc != nil {
			return
		}
		creds, err := google.FindDefaultCredentials(ctx, assetScope)
		if err != nil {
			c.credErr = fmt.Errorf("application default credentials: %w", err)
			return
		}
		// Token refresh uses a background context so it isn't tied to a single
		// request's lifetime; per-request cancellation rides on the request ctx.
		c.hc = oauth2.NewClient(context.Background(), creds.TokenSource)
	})
	return c.hc, c.credErr
}

// searchResponse is the searchAllResources envelope.
type searchResponse struct {
	Results       []resource `json:"results"`
	NextPageToken string     `json:"nextPageToken"`
}

// searchAllResources fetches one page of resources under scope. quotaProject,
// when set, becomes the X-Goog-User-Project header — required when calling with
// gcloud *user* ADC (a service account carries its own billing project), and
// otherwise the call 403s with "API requires a quota project".
func (c *client) searchAllResources(ctx context.Context, scope, pageToken, quotaProject string) (*searchResponse, error) {
	hc, err := c.authedClient(ctx)
	if err != nil {
		return nil, err
	}
	// scope ("projects/x") is a hierarchical path segment, ':searchAllResources'
	// the custom-method verb — neither is URL-escaped.
	endpoint := c.baseURL + "/v1/" + scope + ":searchAllResources"
	q := url.Values{}
	q.Set("pageSize", pageSize)
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if quotaProject != "" {
		req.Header.Set("X-Goog-User-Project", quotaProject)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET searchAllResources: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseAPIError(resp)
	}
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode searchAllResources: %w", err)
	}
	return &out, nil
}

// apiError carries a non-2xx Google API response ({"error":{code,message,status}}).
type apiError struct {
	Code    int
	Status  string
	Message string
}

func (e *apiError) Error() string {
	switch {
	case e.Message != "" && e.Status != "":
		return fmt.Sprintf("gcp API %d (%s): %s", e.Code, e.Status, e.Message)
	case e.Message != "":
		return fmt.Sprintf("gcp API %d: %s", e.Code, e.Message)
	default:
		return fmt.Sprintf("gcp API %d", e.Code)
	}
}

func parseAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	code := env.Error.Code
	if code == 0 {
		code = resp.StatusCode
	}
	return &apiError{Code: code, Status: env.Error.Status, Message: env.Error.Message}
}
