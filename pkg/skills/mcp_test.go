package skills

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mcpCallText drives a tools/call and returns the tool's text output.
func mcpCallText(t *testing.T, agg *Aggregator, name string, args string) string {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	agg.HandleMCP(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("mcp call status = %d", w.Result().StatusCode)
	}
	var resp map[string]any
	json.NewDecoder(w.Result().Body).Decode(&resp)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object: %v", resp)
	}
	return result["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestHandleMCPListTools(t *testing.T) {
	agg := NewAggregator("/tmp")
	agg.tools["test_tool"] = Tool{Name: "test_tool"}

	reqBody := []byte(`{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	agg.HandleMCP(w, req)

	res := w.Result()
	if res.StatusCode != http.StatusOK {
		t.Errorf("expected OK, got %d", res.StatusCode)
	}

	var resp map[string]interface{}
	json.NewDecoder(res.Body).Decode(&resp)

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected result object")
	}

	tools, ok := result["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Errorf("expected 1 tool in response")
	}
}

// TestHandleMCPCallTool_HTTPTemplating drives an http tool with a {piece} path param: the
// arguments must land in the route path and the daemon's response body is the tool output.
// This is how delightd's own furnish tools dispatch.
func TestHandleMCPCallTool_HTTPTemplating(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Write([]byte(`{"applied":true}`))
	}))
	defer srv.Close()

	agg := NewAggregator("/tmp")
	agg.tools["delightd_furnish_up"] = Tool{
		Name:    "delightd_furnish_up",
		Handler: HandlerDef{Type: "http", Method: "POST", URL: srv.URL + "/furnish/{piece}/up"},
	}

	text := mcpCallText(t, agg, "delightd_furnish_up", `{"piece":"surrealdb"}`)
	if gotMethod != http.MethodPost || gotPath != "/furnish/surrealdb/up" {
		t.Errorf("dispatch hit %s %s, want POST /furnish/surrealdb/up", gotMethod, gotPath)
	}
	if text != `{"applied":true}` {
		t.Errorf("tool output = %q, want the daemon body", text)
	}
}

// TestHandleMCPCallTool_MissingArg: a required path param that is not supplied is reported,
// never dispatched as a malformed URL.
func TestHandleMCPCallTool_MissingArg(t *testing.T) {
	agg := NewAggregator("/tmp")
	agg.tools["delightd_furnish_up"] = Tool{
		Name:    "delightd_furnish_up",
		Handler: HandlerDef{Type: "http", Method: "POST", URL: "http://127.0.0.1:8088/furnish/{piece}/up"},
	}
	text := mcpCallText(t, agg, "delightd_furnish_up", `{}`)
	if !strings.Contains(text, "unfilled path parameter") {
		t.Errorf("missing piece should report the unfilled parameter, got: %s", text)
	}
}

func TestHandleMCPInvalidMethod(t *testing.T) {
	agg := NewAggregator("/tmp")
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	w := httptest.NewRecorder()
	agg.HandleMCP(w, req)

	if w.Result().StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected MethodNotAllowed")
	}
}
