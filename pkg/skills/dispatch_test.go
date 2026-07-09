package skills

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestURLTemplateParams checks the distinct, first-seen parameter order the CLI relies on.
func TestURLTemplateParams(t *testing.T) {
	if got := parseURLTemplate("http://h/furnish/{piece}/up").params(); len(got) != 1 || got[0] != "piece" {
		t.Errorf("params = %v, want [piece]", got)
	}
	if p := parseURLTemplate("http://h/x/{a}/{b}/{a}").params(); len(p) != 2 || p[0] != "a" || p[1] != "b" {
		t.Errorf("params distinct/order = %v, want [a b]", p)
	}
	if p := parseURLTemplate("http://h/health").params(); len(p) != 0 {
		t.Errorf("params on a plain URL = %v, want none", p)
	}
}

// TestURLTemplateRenderArgs checks named substitution, escaping, and the missing-arg error.
func TestURLTemplateRenderArgs(t *testing.T) {
	out, err := parseURLTemplate("http://h/furnish/{piece}/up").renderArgs(map[string]any{"piece": "surrealdb"})
	if err != nil || out != "http://h/furnish/surrealdb/up" {
		t.Errorf("renderArgs = %q, %v", out, err)
	}
	out, _ = parseURLTemplate("http://h/p/{name}").renderArgs(map[string]any{"name": "a b/c"})
	if out != "http://h/p/a%20b%2Fc" {
		t.Errorf("renderArgs escaping = %q", out)
	}
	if _, err := parseURLTemplate("http://h/{x}").renderArgs(map[string]any{}); err == nil {
		t.Error("renderArgs with a missing param: want error")
	}
}

// TestDoHTTP_Non2xxIsError checks that a non-2xx status is an error while still returning
// the daemon's body (a monitor gates on the error; an agent still sees the ladder).
func TestDoHTTP_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"healthy":false}`))
	}))
	defer srv.Close()
	body, err := doHTTP(http.MethodGet, srv.URL)
	if err == nil {
		t.Error("doHTTP on a 503: want error")
	}
	if body != `{"healthy":false}` {
		t.Errorf("doHTTP body = %q, want the daemon body", body)
	}
}
