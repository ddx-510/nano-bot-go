package tools

import "encoding/json"

// Tool is the interface every tool must implement.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage // JSON Schema
	Execute(args map[string]any) (string, error)
}

// Schema converts a Tool to the OpenAI function-calling format.
func Schema(t Tool) map[string]any {
	var params any
	_ = json.Unmarshal(t.Parameters(), &params)
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters":  params,
		},
	}
}
