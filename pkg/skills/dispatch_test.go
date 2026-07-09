package skills

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRenderURL checks named substitution, escaping, and the unfilled-placeholder error.
func TestRenderURL(t *testing.T) {
	out, err := renderURL("http://h/furnish/{piece}/up", map[string]any{"piece": "surrealdb"})
	if err != nil || out != "http://h/furnish/surrealdb/up" {
		t.Errorf("renderURL = %q, %v", out, err)
	}
	out, _ = renderURL("http://h/p/{name}", map[string]any{"name": "a b/c"})
	if out != "http://h/p/a%20b%2Fc" {
		t.Errorf("renderURL escaping = %q", out)
	}
	if _, err := renderURL("http://h/{x}", map[string]any{}); err == nil {
		t.Error("renderURL with a missing param: want error")
	}
}

// TestPositionalURL checks the CLI's {name} -> $N rewrite in order of appearance.
func TestPositionalURL(t *testing.T) {
	if got := positionalURL("http://h/furnish/{piece}/up"); got != "http://h/furnish/$1/up" {
		t.Errorf("positionalURL = %q, want .../$1/up", got)
	}
	if got := positionalURL("http://h/x/{a}/{b}"); got != "http://h/x/$1/$2" {
		t.Errorf("positionalURL two params = %q, want $1/$2", got)
	}
	if got := positionalURL("http://h/health"); got != "http://h/health" {
		t.Errorf("positionalURL plain = %q, want unchanged", got)
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
	body, err := doHTTP(http.MethodGet, srv.URL, "")
	if err == nil {
		t.Error("doHTTP on a 503: want error")
	}
	if body != `{"healthy":false}` {
		t.Errorf("doHTTP body = %q, want the daemon body", body)
	}
}
