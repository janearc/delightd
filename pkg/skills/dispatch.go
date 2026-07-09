package skills

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// urlTemplate is a handler URL parsed into literal runs and {name} path parameters, so it
// can be rendered from named arguments (MCP) or positional ones (the generated CLI)
// without rescanning the string on every call.
type urlTemplate struct {
	segs []segment
}

// segment is one piece of a parsed template: a literal run, or a {param} placeholder (in
// which case literal is empty).
type segment struct {
	literal string
	param   string
}

// parseURLTemplate splits raw into literal and {name} segments by scanning for braces. An
// unmatched brace is treated as a literal.
func parseURLTemplate(raw string) urlTemplate {
	var segs []segment
	for len(raw) > 0 {
		open := strings.IndexByte(raw, '{')
		if open < 0 {
			segs = append(segs, segment{literal: raw})
			break
		}
		end := strings.IndexByte(raw[open:], '}')
		if end < 0 {
			segs = append(segs, segment{literal: raw})
			break
		}
		end += open
		if open > 0 {
			segs = append(segs, segment{literal: raw[:open]})
		}
		segs = append(segs, segment{param: raw[open+1 : end]})
		raw = raw[end+1:]
	}
	return urlTemplate{segs: segs}
}

// params returns the distinct parameter names in first-seen order -- the order the
// generated CLI maps positional args onto.
func (t urlTemplate) params() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range t.segs {
		if s.param != "" && !seen[s.param] {
			seen[s.param] = true
			out = append(out, s.param)
		}
	}
	return out
}

// render substitutes each parameter with resolve(name); an error from resolve (e.g. a
// missing argument) stops rendering and is returned.
func (t urlTemplate) render(resolve func(name string) (string, error)) (string, error) {
	var b strings.Builder
	for _, s := range t.segs {
		if s.param == "" {
			b.WriteString(s.literal)
			continue
		}
		v, err := resolve(s.param)
		if err != nil {
			return "", err
		}
		b.WriteString(v)
	}
	return b.String(), nil
}

// renderArgs fills the template from named MCP arguments, path-escaping each value. A
// missing required parameter is an error, never a silently malformed URL.
func (t urlTemplate) renderArgs(args map[string]any) (string, error) {
	return t.render(func(name string) (string, error) {
		v, ok := args[name]
		if !ok {
			return "", fmt.Errorf("missing required argument %q", name)
		}
		return url.PathEscape(fmt.Sprint(v)), nil
	})
}

// doHTTP issues one request to a rendered URL and returns the response body. A non-2xx
// status is a non-nil error, mirroring the success path (body, nil); the body is returned
// in both cases so the daemon's own answer -- e.g. a 503 health ladder -- is never lost.
func doHTTP(method, rawURL string) (string, error) {
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
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
