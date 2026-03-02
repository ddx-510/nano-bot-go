package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ServiceInfo struct {
	BaseURL string
	Token   string
}

// QueryAPITool sends read-only GET requests to internal services.
type QueryAPITool struct {
	Services map[string]ServiceInfo
}

func (t *QueryAPITool) Name() string { return "query_api" }
func (t *QueryAPITool) Description() string {
	names := make([]string, 0, len(t.Services))
	for n := range t.Services {
		names = append(names, n)
	}
	return fmt.Sprintf("Send a read-only GET request to an internal service. Available: %s. READ-ONLY only.", strings.Join(names, ", "))
}
func (t *QueryAPITool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"service": {"type": "string", "description": "Service name (e.g. 'ccmonet-go', 'curiosity')"},
			"path": {"type": "string", "description": "API path (e.g. '/api/v3/health')"},
			"params": {"type": "object", "description": "Query parameters"}
		},
		"required": ["service", "path"]
	}`)
}

func (t *QueryAPITool) Execute(args map[string]any) (string, error) {
	service, _ := args["service"].(string)
	path, _ := args["path"].(string)

	svc, ok := t.Services[service]
	if !ok {
		names := make([]string, 0, len(t.Services))
		for n := range t.Services {
			names = append(names, n)
		}
		return fmt.Sprintf("Error: unknown service '%s'. Available: %s", service, strings.Join(names, ", ")), nil
	}

	u := strings.TrimRight(svc.BaseURL, "/") + path

	// Add query params
	if params, ok := args["params"].(map[string]any); ok && len(params) > 0 {
		parts := make([]string, 0)
		for k, v := range params {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		u += "?" + strings.Join(parts, "&")
	}

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	if svc.Token != "" {
		req.Header.Set("Authorization", "Bearer "+svc.Token)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("Error connecting to %s: %v", service, err), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if len(text) > 15000 {
		text = text[:15000] + "\n... (truncated)"
	}

	if resp.StatusCode != 200 {
		return fmt.Sprintf("HTTP %d\n%s", resp.StatusCode, text), nil
	}
	return text, nil
}
