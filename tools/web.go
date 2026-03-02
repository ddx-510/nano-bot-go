package tools

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebSearchTool searches the web via Brave Search API.
type WebSearchTool struct{ APIKey string }

func (t *WebSearchTool) Name() string        { return "web_search" }
func (t *WebSearchTool) Description() string { return "Search the web using Brave Search API." }
func (t *WebSearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query"},
			"count": {"type": "integer", "description": "Number of results (default 5)"}
		},
		"required": ["query"]
	}`)
}

func (t *WebSearchTool) Execute(args map[string]any) (string, error) {
	query, _ := args["query"].(string)
	if t.APIKey == "" {
		return "Error: Brave API key not configured", nil
	}

	count := intArg(args, "count", 5)
	u := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
		url.QueryEscape(query), count)

	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", t.APIKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("Error: HTTP %d", resp.StatusCode), nil
	}

	var data struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return fmt.Sprintf("Error parsing results: %v", err), nil
	}

	var sb strings.Builder
	for _, r := range data.Web.Results {
		sb.WriteString(fmt.Sprintf("**%s**\n  %s\n  %s\n\n", r.Title, r.URL, r.Description))
	}
	if sb.Len() == 0 {
		return "No results found.", nil
	}
	return sb.String(), nil
}

// WebFetchTool fetches a URL and returns the content.
type WebFetchTool struct{}

func (t *WebFetchTool) Name() string        { return "web_fetch" }
func (t *WebFetchTool) Description() string { return "Fetch a URL and return its content." }
func (t *WebFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "URL to fetch"}
		},
		"required": ["url"]
	}`)
}

func (t *WebFetchTool) Execute(args map[string]any) (string, error) {
	u, _ := args["url"].(string)

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Sprintf("Error: HTTP %d", resp.StatusCode), nil
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if len(text) > 15000 {
		text = text[:15000] + "\n... (truncated)"
	}
	return text, nil
}
