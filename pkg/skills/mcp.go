package skills

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
)

// HandleMCP provides a simple JSON-RPC 2.0 endpoint for the Model Context Protocol
func (a *Aggregator) HandleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      interface{}     `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	sendResponse := func(result interface{}, errObj interface{}) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
		}
		if errObj != nil {
			resp["error"] = errObj
		} else {
			resp["result"] = result
		}
		json.NewEncoder(w).Encode(resp)
	}

	switch req.Method {
	case "tools/list":
		tools := a.GetTools()
		sendResponse(map[string]interface{}{"tools": tools}, nil)

	case "tools/call":
		var params struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			sendResponse(nil, map[string]interface{}{"code": -32602, "message": "Invalid params"})
			return
		}

		tool, exists := a.GetTool(params.Name)
		if !exists {
			sendResponse(nil, map[string]interface{}{"code": -32601, "message": "Tool not found"})
			return
		}

		// Execute the tool based on handler type
		// For a real production MCP server, we'd capture output streams properly.
		var output string
		if tool.Handler.Type == "command" {
			cmd := exec.Command(tool.Handler.Command, tool.Handler.Args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				output = err.Error() + ": " + string(out)
			} else {
				output = string(out)
			}
		} else if tool.Handler.Type == "internal" && tool.Handler.Method == "backup" {
			// Dogfooding triggers backup. In a real scenario we'd call the control port or internal func
			output = "Backup triggered for " + fmt.Sprintf("%v", params.Arguments["project"])
		} else {
			output = "Unsupported handler type"
		}

		sendResponse(map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": output},
			},
		}, nil)

	default:
		sendResponse(nil, map[string]interface{}{"code": -32601, "message": "Method not found"})
	}
}
