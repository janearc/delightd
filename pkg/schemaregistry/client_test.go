package schemaregistry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSubjectExists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "boom.v1.Thing"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "known.v1.Thing"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	if ok, err := c.SubjectExists(ctx, "known.v1.Thing"); err != nil || !ok {
		t.Fatalf("known subject: ok=%v err=%v, want true/nil", ok, err)
	}
	if ok, err := c.SubjectExists(ctx, "missing.v1.Thing"); err != nil || ok {
		t.Fatalf("missing subject: ok=%v err=%v, want false/nil", ok, err)
	}
	// an SR error (5xx) MUST surface as an error, never a silent "not exists".
	if _, err := c.SubjectExists(ctx, "boom.v1.Thing"); err == nil {
		t.Fatal("SR 500 should return an error, got nil")
	}
}

func TestSubjectExistsUnconfigured(t *testing.T) {
	if _, err := New("").SubjectExists(context.Background(), "x.v1.Y"); err == nil {
		t.Fatal("empty SR URL should error, got nil")
	}
}
