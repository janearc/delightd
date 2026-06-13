package skills

import "encoding/json"

// Tool defines an MCP tool schema
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Handler     HandlerDef      `json:"handler"` // Internal delightd routing logic
}

// HandlerDef tells delightd how to execute the tool
type HandlerDef struct {
	Type    string   `json:"type"`    // "http" or "command"
	Method  string   `json:"method"`  // for http: GET, POST
	URL     string   `json:"url"`     // for http: http://localhost:8081/health
	Command string   `json:"command"` // for command: /var/bin/delight_client.py
	Args    []string `json:"args"`
}

// Manifest represents the structure of an mcp.json file
type Manifest struct {
	Tools []Tool `json:"tools"`
}
