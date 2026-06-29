// Package schemaregistry is a minimal READ client for the Confluent schema registry:
// enough to answer "is this subject registered?" for the /register RULE-4 verification. It
// reads the same SR delightd already publishes to -- the read half of that connection, not a
// new dependency.
package schemaregistry

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a read-only schema-registry client.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Client for the SR at baseURL. A trailing slash is tolerated. An empty URL
// yields a client whose checks fail loudly (an unconfigured SR MUST NOT silently pass).
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 5 * time.Second},
	}
}

// SubjectExists reports whether subject is registered. It GETs /subjects/{subject}/versions:
// 200 means the subject exists, 404 means it does not, and anything else is an error -- so an
// SR outage fails the check loudly rather than silently passing an unverifiable claim.
func (c *Client) SubjectExists(ctx context.Context, subject string) (bool, error) {
	if c.baseURL == "" {
		return false, fmt.Errorf("schema registry URL is not configured")
	}
	u := fmt.Sprintf("%s/subjects/%s/versions", c.baseURL, url.PathEscape(subject))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("schema registry returned %d for subject %q", resp.StatusCode, subject)
	}
}
