package skills

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestHandleMCPCallTool(t *testing.T) {
	agg := NewAggregator("/tmp")
	agg.tools["delightd_trigger_backup"] = Tool{
		Name: "delightd_trigger_backup",
		Handler: HandlerDef{
			Type:   "internal",
			Method: "backup",
		},
	}

	reqBody := []byte(`{
		"jsonrpc": "2.0", 
		"id": 2, 
		"method": "tools/call", 
		"params": {
			"name": "delightd_trigger_backup",
			"arguments": {"project": "odysseus"}
		}
	}`)
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

	content := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)

	if text != "Backup triggered for odysseus" {
		t.Errorf("unexpected output: %s", text)
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
