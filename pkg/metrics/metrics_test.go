package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetrics(t *testing.T) {
	Reset()

	Inc(`delightd_test_total{status="success"}`)
	Inc(`delightd_test_total{status="success"}`)
	Inc(`delightd_test_total{status="fail"}`)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	Handler().ServeHTTP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("expected OK, got %d", res.StatusCode)
	}

	body, _ := io.ReadAll(res.Body)
	content := string(body)

	if !strings.Contains(content, `delightd_test_total{status="success"} 2`) {
		t.Errorf("missing success counter: %s", content)
	}
	if !strings.Contains(content, `delightd_test_total{status="fail"} 1`) {
		t.Errorf("missing fail counter: %s", content)
	}
}
