// Package client is the HTTP client for a shale hub.
//
// Deliberately thin: encode JSON, set the API key header, decode JSON, turn
// non-2xx into an error carrying the server's own message. This binary is meant
// to break in obvious ways rather than paper over failures, so nothing here
// retries silently — retry policy belongs to the caller draining the spool, where
// it can be reported in `shale status`.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/buildinfo"
)

const (
	// DefaultTimeout suits JSON control-plane calls. Transcript upload and commit
	// override it: commit parses a whole session server-side and the server's own
	// budget for that is 300s.
	DefaultTimeout = 60 * time.Second

	// Server error bodies are small JSON objects; cap the read so a misconfigured
	// URL pointing at something enormous can't exhaust memory.
	maxErrorBody = 8 << 10
)

// APIError carries the server's status and message so callers can distinguish
// "you're not allowed" from "the network is down" — the difference between a
// message the user must act on and one worth retrying.
type APIError struct {
	Status  int
	Message string
	Path    string
	// Code is the server's machine-readable discriminator when it sends one. The
	// capture flow depends on it: a 403 carrying "consent_revoked" is org-wide and
	// should stop the run, while "tier_not_allowed" affects only one repo.
	Code string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s returned HTTP %d", e.Path, e.Status)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Path, e.Message, e.Status)
}

// Retryable reports whether waiting and trying again could plausibly succeed. 4xx
// means the request itself is wrong — retrying just burns the user's time.
func (e *APIError) Retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Client talks to one hub.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// New returns a client with the default timeout.
func New(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: DefaultTimeout},
	}
}

// PostJSON sends body to BaseURL+path and decodes the response into out (which
// may be nil when the response is uninteresting).
func (c *Client) PostJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cannot encode request for %s: %w", path, err)
	}
	return c.do(ctx, http.MethodPost, path, "application/json", payload, nil, out)
}

// GetJSON fetches path and decodes into out.
func (c *Client) GetJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, "", nil, nil, out)
}

// PutJSON sends body to path with PUT.
func (c *Client) PutJSON(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("cannot encode request for %s: %w", path, err)
	}
	return c.do(ctx, http.MethodPut, path, "application/json", payload, nil, out)
}

// PostBytes sends a raw body with an explicit content type and extra headers —
// the transcript upload path, which ships gzip with capture metadata in headers
// rather than a JSON envelope.
func (c *Client) PostBytes(ctx context.Context, path, contentType string, body []byte, headers map[string]string, out any) error {
	return c.do(ctx, http.MethodPost, path, contentType, body, headers, out)
}

func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte, headers map[string]string, out any) error {
	if c.BaseURL == "" {
		return fmt.Errorf("no hub URL configured — run `%s link`", buildinfo.Name)
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("cannot build request for %s: %w", path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", buildinfo.UserAgent())
	if c.APIKey != "" {
		req.Header.Set("x-api-key", c.APIKey)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, code := errorMessage(resp.Body)
		return &APIError{Status: resp.StatusCode, Message: msg, Path: path, Code: code}
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s returned a response this client could not read: %w", path, err)
	}
	return nil
}

// errorMessage pulls the server's `error` field out of a failure response,
// falling back to the raw body, and returns any machine-readable `code`.
// Surfacing the server's own wording matters: "session_capture not consented"
// tells the user what to do, "HTTP 403" does not.
func errorMessage(r io.Reader) (message, code string) {
	raw, err := io.ReadAll(io.LimitReader(r, maxErrorBody))
	if err != nil || len(raw) == 0 {
		return "", ""
	}
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		switch {
		case payload.Error != "":
			return payload.Error, payload.Code
		case payload.Message != "":
			return payload.Message, payload.Code
		}
	}
	return strings.TrimSpace(string(raw)), ""
}

// AsAPIError unwraps err into target when it is an *APIError. A thin wrapper over
// errors.As so callers don't each need the import.
func AsAPIError(err error, target **APIError) bool { return errors.As(err, target) }
