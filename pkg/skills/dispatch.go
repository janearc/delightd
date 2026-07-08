package skills

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// paramRe matches a {name} path parameter in a handler URL. delightd's own tools use
// these so an agent can name a piece or project and have it land in the route path
// (e.g. /furnish/{piece}/up), which is what an operator surface needs.
var paramRe = regexp.MustCompile(`\{(\w+)\}`)

// urlParams returns the distinct {name} parameters in a URL template, in first-seen order
// -- the order the generated CLI wrapper maps positional args onto.
func urlParams(tmpl string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range paramRe.FindAllStringSubmatch(tmpl, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// renderURL substitutes each {name} in the template with the (path-escaped) named
// argument, for the MCP dispatch path where arguments arrive as a map. A missing required
// parameter is an error, not a silently malformed URL.
func renderURL(tmpl string, args map[string]any) (string, error) {
	var missing []string
	out := paramRe.ReplaceAllStringFunc(tmpl, func(tok string) string {
		key := tok[1 : len(tok)-1]
		v, ok := args[key]
		if !ok {
			missing = append(missing, key)
			return tok
		}
		return url.PathEscape(fmt.Sprint(v))
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("missing required argument(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// doHTTP issues one request to a rendered URL and returns the response body as text. A
// non-2xx status is folded into the returned text with the status, so an agent sees the
// daemon's own error (e.g. a 503 furnish health ladder) rather than a bare failure.
func doHTTP(method, rawURL string) string {
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return "bad request: " + err.Error()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "request failed: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := strings.TrimSpace(string(b))
	if resp.StatusCode/100 != 2 {
		return fmt.Sprintf("%s: %s", resp.Status, body)
	}
	return body
}
