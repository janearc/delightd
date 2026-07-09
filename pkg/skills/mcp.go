package skills

import (
	"encoding/json"
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

		// Execute the tool based on handler type.
		var output string
		switch tool.Handler.Type {
		case "command":
			cmd := exec.Command(tool.Handler.Command, tool.Handler.Args...)
			out, err := cmd.CombinedOutput()
			if err != nil {
				output = err.Error() + ": " + string(out)
			} else {
				output = string(out)
			}
		case "http":
			// Render {name} path params from the call arguments, then hit the control
			// port (or any URL the tool names). This is how delightd's own tools -- and
			// any fleet tool with a parameterized route -- dispatch. The daemon's body is
			// the tool output; a non-2xx that carries a body (e.g. a 503 health ladder) is
			// still returned, and a bodiless failure surfaces its error.
			rendered, err := renderURL(tool.Handler.URL, params.Arguments)
			if err != nil {
				output = err.Error()
				break
			}
			// Attach the control-port bearer only for a mutation aimed at the loopback control
			// port (the bearer-gated routes). Never send delightd's secret to a foreign host a
			// tool names, and a read needs no credential.
			var bearer string
			if isMutatingMethod(tool.Handler.Method) && isLoopbackURL(rendered) {
				a.mu.RLock()
				bearer = a.controlToken
				a.mu.RUnlock()
			}
			resp, err := doHTTP(tool.Handler.Method, rendered, bearer)
			output = resp
			if err != nil && resp == "" {
				output = err.Error()
			}
		default:
			output = "unsupported handler type: " + tool.Handler.Type
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
