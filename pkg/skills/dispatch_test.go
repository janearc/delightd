package skills

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestURLParams(t *testing.T) {
	got := urlParams("http://h/furnish/{piece}/up")
	if len(got) != 1 || got[0] != "piece" {
		t.Errorf("urlParams = %v, want [piece]", got)
	}
	// distinct, first-seen order; a plain URL has none.
	if p := urlParams("http://h/x/{a}/{b}/{a}"); len(p) != 2 || p[0] != "a" || p[1] != "b" {
		t.Errorf("urlParams distinct/order = %v, want [a b]", p)
	}
	if p := urlParams("http://h/health"); len(p) != 0 {
		t.Errorf("urlParams on a plain URL = %v, want none", p)
	}
}

func TestRenderURL(t *testing.T) {
	out, err := renderURL("http://h/furnish/{piece}/up", map[string]any{"piece": "surrealdb"})
	if err != nil || out != "http://h/furnish/surrealdb/up" {
		t.Errorf("renderURL = %q, %v", out, err)
	}
	// a value that needs escaping stays path-safe.
	out, _ = renderURL("http://h/p/{name}", map[string]any{"name": "a b/c"})
	if out != "http://h/p/a%20b%2Fc" {
		t.Errorf("renderURL escaping = %q", out)
	}
	// a missing required param is an error, not a malformed URL.
	if _, err := renderURL("http://h/{x}", map[string]any{}); err == nil {
		t.Error("renderURL with a missing param: want error")
	}
}

func TestDoHTTP_FoldsNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"healthy":false}`))
	}))
	defer srv.Close()
	out := doHTTP(http.MethodGet, srv.URL)
	// A 503 (a RED/INDETERMINATE health) is surfaced with the status and body, so the
	// agent sees the daemon's own answer.
	if out == `{"healthy":false}` || !contains(out, "503") || !contains(out, "healthy") {
		t.Errorf("doHTTP non-2xx = %q, want status + body", out)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
