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

// mcpCallResult is mcpCallText plus the isError flag -- the M4 contract (sprints#58) is
// that a failed backing execution reaches the agent as isError=true, never as a success.
func mcpCallResult(t *testing.T, agg *Aggregator, name string, args string) (string, bool) {
	t.Helper()
	body := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	w := httptest.NewRecorder()
	agg.HandleMCP(w, req)
	var resp map[string]any
	json.NewDecoder(w.Result().Body).Decode(&resp)
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result object: %v", resp)
	}
	isErr, _ := result["isError"].(bool)
	return result["content"].([]any)[0].(map[string]any)["text"].(string), isErr
}

// TestHandleMCPCallTool_HTTPFailureIsError: a 503-with-body from the daemon must surface
// as isError=true with the body preserved -- an agent calling furnish_health on a RED
// piece must not receive a successful tool result.
func TestHandleMCPCallTool_HTTPFailureIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"healthy":false,"pieces":{"kafka":"red"}}`))
	}))
	defer srv.Close()

	agg := NewAggregator("/tmp")
	agg.tools["delightd_furnish_health"] = Tool{
		Name:    "delightd_furnish_health",
		Handler: HandlerDef{Type: "http", Method: "GET", URL: srv.URL + "/furnish/health"},
	}
	text, isErr := mcpCallResult(t, agg, "delightd_furnish_health", `{}`)
	if !isErr {
		t.Error("503 from the daemon must set isError=true")
	}
	if text != `{"healthy":false,"pieces":{"kafka":"red"}}` {
		t.Errorf("failure body not preserved as error content: %q", text)
	}
}

// TestHandleMCPCallTool_SuccessNoIsError: a 2xx dispatch stays a plain success.
func TestHandleMCPCallTool_SuccessNoIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"healthy":true}`))
	}))
	defer srv.Close()

	agg := NewAggregator("/tmp")
	agg.tools["delightd_furnish_health"] = Tool{
		Name:    "delightd_furnish_health",
		Handler: HandlerDef{Type: "http", Method: "GET", URL: srv.URL + "/furnish/health"},
	}
	if _, isErr := mcpCallResult(t, agg, "delightd_furnish_health", `{}`); isErr {
		t.Error("2xx must not set isError")
	}
}

// TestHandleMCPCallTool_CommandFailureIsError: a command tool whose process exits nonzero
// is a tool error, not a success with sad text.
func TestHandleMCPCallTool_CommandFailureIsError(t *testing.T) {
	agg := NewAggregator("/tmp")
	agg.tools["example_fail"] = Tool{
		Name:    "example_fail",
		Handler: HandlerDef{Type: "command", Command: "sh", Args: []string{"-c", "echo doomed; exit 3"}},
	}
	text, isErr := mcpCallResult(t, agg, "example_fail", `{}`)
	if !isErr {
		t.Error("nonzero command exit must set isError=true")
	}
	if !strings.Contains(text, "doomed") {
		t.Errorf("command output not preserved in error content: %q", text)
	}
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

// TestHandleMCPCallTool_MutatingSendsBearer: a mutating self-tool dispatched to the loopback
// control port carries the wired control-port bearer, so the daemon's own MCP surface can drive
// its bearer-gated mutations. httptest binds 127.0.0.1, the loopback the bearer is scoped to.
func TestHandleMCPCallTool_MutatingSendsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"applied":true}`))
	}))
	defer srv.Close()

	agg := NewAggregator("/tmp")
	agg.UseControlToken("s3cr3t")
	agg.tools["delightd_furnish_up"] = Tool{
		Name:    "delightd_furnish_up",
		Handler: HandlerDef{Type: "http", Method: "POST", URL: srv.URL + "/furnish/{piece}/up"},
	}
	mcpCallText(t, agg, "delightd_furnish_up", `{"piece":"surrealdb"}`)
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("mutating dispatch Authorization = %q, want %q", gotAuth, "Bearer s3cr3t")
	}
}

// TestHandleMCPCallTool_ReadSendsNoBearer: a read dispatch never carries the bearer.
func TestHandleMCPCallTool_ReadSendsNoBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"pieces":[]}`))
	}))
	defer srv.Close()

	agg := NewAggregator("/tmp")
	agg.UseControlToken("s3cr3t")
	agg.tools["delightd_furnish_list"] = Tool{
		Name:    "delightd_furnish_list",
		Handler: HandlerDef{Type: "http", Method: "GET", URL: srv.URL + "/furnish/pieces"},
	}
	mcpCallText(t, agg, "delightd_furnish_list", `{}`)
	if gotAuth != "" {
		t.Errorf("read dispatch sent Authorization = %q, want none", gotAuth)
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
