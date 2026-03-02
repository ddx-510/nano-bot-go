package tools

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// McpServer represents a configured MCP server endpoint.
type McpServer struct {
	Name string
	URL  string // HTTP endpoint
	Cmd  string // stdio command
}

// McpConnection holds a live connection to an MCP server (stdio).
type McpConnection struct {
	Server  McpServer
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	mu      sync.Mutex
	nextID  atomic.Int64
}

// mcpToolDef represents a tool discovered from an MCP server.
type mcpToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// McpNativeTool wraps a single MCP server tool as a native registry tool.
type McpNativeTool struct {
	ServerName  string
	ToolName    string
	Desc        string
	Schema      json.RawMessage
	conn        *McpConnection // for stdio
	httpURL     string         // for HTTP
	httpNextID  *atomic.Int64
}

func (t *McpNativeTool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", t.ServerName, t.ToolName)
}

func (t *McpNativeTool) Description() string {
	return t.Desc
}

func (t *McpNativeTool) Parameters() json.RawMessage {
	if t.Schema != nil {
		return t.Schema
	}
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *McpNativeTool) Execute(args map[string]any) (string, error) {
	params := map[string]any{
		"name":      t.ToolName,
		"arguments": args,
	}

	var result json.RawMessage
	var err error

	if t.conn != nil {
		result, err = t.conn.Call("tools/call", params)
	} else if t.httpURL != "" {
		id := t.httpNextID.Add(1)
		result, err = sendHTTP(t.httpURL, id, "tools/call", params)
	} else {
		return "Error: MCP tool has no connection", nil
	}

	if err != nil {
		return fmt.Sprintf("Error: %v", err), nil
	}
	return formatMcpResult(result), nil
}

// ConnectMcpServers connects to all MCP servers and registers their tools.
// Call this on startup. Returns tools to register.
func ConnectMcpServers(servers []McpServer) []Tool {
	var allTools []Tool

	for _, server := range servers {
		if server.Cmd != "" {
			tools := connectStdio(server)
			allTools = append(allTools, tools...)
		} else if server.URL != "" {
			tools := connectHTTP(server)
			allTools = append(allTools, tools...)
		}
	}

	return allTools
}

func connectStdio(server McpServer) []Tool {
	parts := strings.Fields(server.Cmd)
	if len(parts) == 0 {
		log.Printf("[mcp] empty command for server '%s'", server.Name)
		return nil
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("[mcp] %s: stdin pipe error: %v", server.Name, err)
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[mcp] %s: stdout pipe error: %v", server.Name, err)
		return nil
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[mcp] %s: start error: %v", server.Name, err)
		return nil
	}

	conn := &McpConnection{
		Server:  server,
		cmd:     cmd,
		stdin:   stdin,
		scanner: bufio.NewScanner(stdout),
	}
	conn.scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	// Initialize
	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "monet-bot", "version": "0.1.0"},
	}
	_, err = conn.Call("initialize", initParams)
	if err != nil {
		log.Printf("[mcp] %s: init error: %v", server.Name, err)
		cmd.Process.Kill()
		return nil
	}

	// Send initialized notification
	notif, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	conn.mu.Lock()
	conn.stdin.Write(append(notif, '\n'))
	conn.mu.Unlock()

	// List tools
	result, err := conn.Call("tools/list", nil)
	if err != nil {
		log.Printf("[mcp] %s: list tools error: %v", server.Name, err)
		cmd.Process.Kill()
		return nil
	}

	return parseMcpTools(server.Name, result, conn, "", nil)
}

func connectHTTP(server McpServer) []Tool {
	var nextID atomic.Int64

	id := nextID.Add(1)
	// Initialize
	initParams := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "monet-bot", "version": "0.1.0"},
	}
	_, err := sendHTTP(server.URL, id, "initialize", initParams)
	if err != nil {
		log.Printf("[mcp] %s: HTTP init error: %v", server.Name, err)
		return nil
	}

	// List tools
	id = nextID.Add(1)
	result, err := sendHTTP(server.URL, id, "tools/list", nil)
	if err != nil {
		log.Printf("[mcp] %s: HTTP list tools error: %v", server.Name, err)
		return nil
	}

	return parseMcpTools(server.Name, result, nil, server.URL, &nextID)
}

func parseMcpTools(serverName string, result json.RawMessage, conn *McpConnection, httpURL string, httpNextID *atomic.Int64) []Tool {
	var listResult struct {
		Tools []mcpToolDef `json:"tools"`
	}
	if err := json.Unmarshal(result, &listResult); err != nil {
		log.Printf("[mcp] %s: parse tools error: %v", serverName, err)
		return nil
	}

	var tools []Tool
	for _, td := range listResult.Tools {
		t := &McpNativeTool{
			ServerName: serverName,
			ToolName:   td.Name,
			Desc:       td.Description,
			Schema:     td.InputSchema,
			conn:       conn,
			httpURL:    httpURL,
			httpNextID: httpNextID,
		}
		tools = append(tools, t)
		log.Printf("[mcp] registered: %s (from %s)", t.Name(), serverName)
	}
	log.Printf("[mcp] %s: connected, %d tools registered", serverName, len(tools))
	return tools
}

// Call sends a JSON-RPC request and waits for the response (stdio).
func (c *McpConnection) Call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID.Add(1)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}

	payload, _ := json.Marshal(req)
	payload = append(payload, '\n')
	if _, err := c.stdin.Write(payload); err != nil {
		return nil, fmt.Errorf("write error: %w", err)
	}

	// Read response (with timeout via channel)
	type scanResult struct {
		line string
		ok   bool
	}
	ch := make(chan scanResult, 1)
	go func() {
		ok := c.scanner.Scan()
		ch <- scanResult{c.scanner.Text(), ok}
	}()

	select {
	case sr := <-ch:
		if !sr.ok {
			return nil, fmt.Errorf("server closed connection")
		}
		var resp struct {
			Result json.RawMessage `json:"result,omitempty"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error,omitempty"`
		}
		if err := json.Unmarshal([]byte(sr.line), &resp); err != nil {
			return nil, fmt.Errorf("parse error: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("timeout waiting for response")
	}
}

func sendHTTP(url string, id int64, method string, params any) (json.RawMessage, error) {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var rpcResp struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, nil
}

// formatMcpResult extracts text content from MCP tool call results.
func formatMcpResult(data json.RawMessage) string {
	if data == nil {
		return "(no result)"
	}

	// Try to extract content array (standard MCP format)
	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &callResult); err == nil && len(callResult.Content) > 0 {
		var parts []string
		for _, c := range callResult.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			result := strings.Join(parts, "\n")
			if len(result) > 15000 {
				result = result[:15000] + "\n... (truncated)"
			}
			return result
		}
	}

	// Fallback: pretty-print raw JSON
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return string(data)
	}
	result := pretty.String()
	if len(result) > 15000 {
		result = result[:15000] + "\n... (truncated)"
	}
	return result
}
