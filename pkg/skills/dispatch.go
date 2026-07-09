package skills

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// dispatchClient bounds every dispatched call -- http.DefaultClient has no timeout, so a
// wedged control port would hang the MCP tool silently. The ceiling sits above the
// longest server-side furnish deadline (60s, pkg/httpapi/furnish.go) so the daemon's own
// loud timeout body is relayed to the tool caller rather than clipped by the client.
var dispatchClient = &http.Client{Timeout: 90 * time.Second}

// renderURL fills each {name} placeholder in rawURL with the named argument (path-escaped),
// and reports any placeholder left unfilled as an error rather than a malformed URL. This
// is deliberately a substitution, not a parser: a tool whose arguments need real parsing
// uses a command handler pointing at a cobra CLI (delightd's own `furnish` command is one),
// so this never has to grow into an argument lexer.
func renderURL(rawURL string, args map[string]any) (string, error) {
	for name, v := range args {
		rawURL = strings.ReplaceAll(rawURL, "{"+name+"}", url.PathEscape(fmt.Sprint(v)))
	}
	if strings.ContainsAny(rawURL, "{}") {
		return "", fmt.Errorf("unfilled path parameter in %q", rawURL)
	}
	return rawURL, nil
}

// positionalURL rewrites each {name} placeholder to a positional shell arg ($1, $2, ...) in
// order of appearance, for the generated CLI. Same deliberately-trivial substitution as
// renderURL; not a parser.
func positionalURL(rawURL string) string {
	for i := 1; ; i++ {
		open := strings.IndexByte(rawURL, '{')
		if open < 0 {
			return rawURL
		}
		end := strings.IndexByte(rawURL[open:], '}')
		if end < 0 {
			return rawURL
		}
		rawURL = rawURL[:open] + fmt.Sprintf("$%d", i) + rawURL[open+end+1:]
	}
}

// doHTTP issues one request to a rendered URL and returns the response body. A non-2xx
// status is a non-nil error, mirroring the success path (body, nil); the body is returned
// in both cases so the daemon's own answer -- e.g. a 503 health ladder -- is never lost.
// A non-empty bearer is sent as Authorization: Bearer, so a mutating dispatch to the
// bearer-gated control port authenticates; the caller decides when a bearer applies (never
// attach one to a foreign host -- see mcp.go).
func doHTTP(method, rawURL, bearer string) (string, error) {
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return "", err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := dispatchClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(b))
	if resp.StatusCode/100 != 2 {
		return body, fmt.Errorf("control port returned %s", resp.Status)
	}
	return body, nil
}

// isMutatingMethod reports whether an HTTP method changes state. Used to decide when a
// dispatch (or a generated CLI call) must carry the control-port bearer.
func isMutatingMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// isLoopbackURL reports whether rawURL targets the loopback address -- i.e. delightd's own
// control port. The bearer is attached only for these: it is delightd's secret and must never
// be sent to another fleet host a tool happens to name.
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
