package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ToolCall represents a function call requested by the LLM.
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Response from the LLM.
type Response struct {
	Content   string
	ToolCalls []ToolCall
}

func (r *Response) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// Provider talks to the LLM via OpenAI-compatible API.
// Works with OpenRouter, OpenAI, and any compatible endpoint.
type Provider struct {
	APIKey       string
	BaseURL      string
	Model        string
	ProviderName string // original provider name for feature detection
}

// ---------------------------------------------------------------------------
// Provider spec registry for auto-detection
// ---------------------------------------------------------------------------

// providerSpec describes a known LLM provider for auto-detection.
type providerSpec struct {
	name        string   // canonical name
	keywords    []string // model-name keywords that indicate this provider
	keyPrefixes []string // API key prefixes for auto-detection
	defaultBase string   // default base URL
}

var providerRegistry = []providerSpec{
	{
		name:        "openrouter",
		keywords:    []string{"openrouter"},
		keyPrefixes: []string{"sk-or-"},
		defaultBase: "https://openrouter.ai/api/v1",
	},
	{
		name:        "openai",
		keywords:    []string{"gpt", "o1", "o3", "o4"},
		keyPrefixes: []string{"sk-proj-"},
		defaultBase: "https://api.openai.com/v1",
	},
	{
		name:        "anthropic",
		keywords:    []string{"claude"},
		keyPrefixes: []string{"sk-ant-"},
		defaultBase: "https://api.anthropic.com/v1",
	},
	{
		name:        "gemini",
		keywords:    []string{"gemini"},
		keyPrefixes: []string{"AIza"},
		defaultBase: "https://generativelanguage.googleapis.com/v1beta/openai",
	},
	{
		name:        "deepseek",
		keywords:    []string{"deepseek"},
		keyPrefixes: []string{"sk-ds-"},
		defaultBase: "https://api.deepseek.com/v1",
	},
	{
		name:        "groq",
		keywords:    []string{"groq"},
		keyPrefixes: []string{"gsk_"},
		defaultBase: "https://api.groq.com/openai/v1",
	},
	{
		name:        "dashscope",
		keywords:    []string{"qwen", "dashscope"},
		keyPrefixes: []string{"sk-dash-"},
		defaultBase: "https://dashscope.aliyuncs.com/compatible-mode/v1",
	},
	{
		name:        "moonshot",
		keywords:    []string{"moonshot", "kimi"},
		keyPrefixes: []string{"sk-moon-"},
		defaultBase: "https://api.moonshot.cn/v1",
	},
	{
		name:        "siliconflow",
		keywords:    []string{"siliconflow"},
		keyPrefixes: []string{"sk-sf-"},
		defaultBase: "https://api.siliconflow.cn/v1",
	},
	{
		name:        "volcengine",
		keywords:    []string{"volcengine"},
		keyPrefixes: []string{"sk-volc-"},
		defaultBase: "https://open.volcengineapi.com/api/v3",
	},
	{
		name:        "zhipu",
		keywords:    []string{"zhipu", "glm"},
		keyPrefixes: []string{"zhipu-"},
		defaultBase: "https://open.bigmodel.cn/api/paas/v4",
	},
	{
		name:        "minimax",
		keywords:    []string{"minimax"},
		keyPrefixes: []string{"mm-"},
		defaultBase: "https://api.minimax.chat/v1",
	},
}

// New creates a Provider with auto-detection of provider, base URL, and model.
// The baseURLOverride, if non-empty, takes precedence over any detected base URL.
func New(apiKey, model, provider string) *Provider {
	return NewWithBaseURL(apiKey, model, provider, "")
}

// NewWithBaseURL creates a Provider, allowing an explicit base URL override from config.
func NewWithBaseURL(apiKey, model, provider, baseURLOverride string) *Provider {
	resolved := resolveProvider(apiKey, model, provider)

	baseURL := resolved.defaultBase
	if baseURLOverride != "" {
		baseURL = strings.TrimRight(baseURLOverride, "/")
	}

	// Strip provider prefix from model name for direct API calls
	// e.g. "openrouter/arcee-ai/..." stays as-is for OpenRouter
	actualModel := model
	if resolved.name == "openrouter" && strings.HasPrefix(model, "openrouter/") {
		actualModel = strings.TrimPrefix(model, "openrouter/")
	}

	log.Printf("[provider] resolved provider=%s base=%s model=%s", resolved.name, baseURL, actualModel)

	return &Provider{
		APIKey:       apiKey,
		BaseURL:      baseURL,
		Model:        actualModel,
		ProviderName: resolved.name,
	}
}

// resolveProvider determines the provider spec from explicit name, API key prefix, or model keywords.
func resolveProvider(apiKey, model, provider string) providerSpec {
	pLower := strings.ToLower(strings.TrimSpace(provider))
	modelLower := strings.ToLower(model)

	// 1. Explicit provider name match
	if pLower != "" && pLower != "auto" {
		for _, spec := range providerRegistry {
			if strings.Contains(pLower, spec.name) {
				return spec
			}
		}
		// Also check keywords in provider name (e.g. provider="qwen" matches dashscope)
		for _, spec := range providerRegistry {
			for _, kw := range spec.keywords {
				if strings.Contains(pLower, kw) {
					return spec
				}
			}
		}
	}

	// 2. Auto-detect by API key prefix
	for _, spec := range providerRegistry {
		for _, prefix := range spec.keyPrefixes {
			if strings.HasPrefix(apiKey, prefix) {
				return spec
			}
		}
	}

	// 3. Auto-detect by model name keywords
	for _, spec := range providerRegistry {
		for _, kw := range spec.keywords {
			if strings.Contains(modelLower, kw) {
				return spec
			}
		}
	}

	// 4. Fallback to openrouter
	return providerRegistry[0]
}

// ---------------------------------------------------------------------------
// Model-specific parameter overrides (Item #6)
// ---------------------------------------------------------------------------

// modelOverride describes parameter adjustments for specific models.
type modelOverride struct {
	pattern     string   // substring to match in model name (case-insensitive)
	minTemp     *float64 // if set, clamp temperature to at least this value
	defaultTemp *float64 // if set, override temperature to this value
}

func floatPtr(f float64) *float64 { return &f }

var modelOverrides = []modelOverride{
	{
		pattern: "kimi-k2.5",
		minTemp: floatPtr(1.0), // kimi-k2.5 requires temperature >= 1.0
	},
	{
		pattern:     "deepseek-r1",
		defaultTemp: floatPtr(0.6), // reasoning model works best at 0.6
	},
}

// applyModelOverrides adjusts parameters based on known model quirks.
func applyModelOverrides(model string, temperature float64) float64 {
	modelLower := strings.ToLower(model)
	for _, ov := range modelOverrides {
		if strings.Contains(modelLower, strings.ToLower(ov.pattern)) {
			if ov.defaultTemp != nil {
				temperature = *ov.defaultTemp
			}
			if ov.minTemp != nil && temperature < *ov.minTemp {
				temperature = *ov.minTemp
			}
		}
	}
	return temperature
}

// supportsPromptCaching returns true for providers that support Anthropic-style cache_control.
func (p *Provider) supportsPromptCaching() bool {
	pn := strings.ToLower(p.ProviderName)
	return strings.Contains(pn, "anthropic") || strings.Contains(pn, "openrouter")
}

// applyCacheControl adds cache_control to system messages and the last tool
// for providers that support Anthropic-style prompt caching.
func applyCacheControl(messages []map[string]any, tools []map[string]any) ([]map[string]any, []map[string]any) {
	// Add cache_control to system messages
	cachedMsgs := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		role, _ := msg["role"].(string)
		if role == "system" {
			m := copyMap(msg)
			// Convert string content to content block array with cache_control
			if text, ok := m["content"].(string); ok {
				m["content"] = []map[string]any{
					{
						"type":          "text",
						"text":          text,
						"cache_control": map[string]string{"type": "ephemeral"},
					},
				}
			}
			cachedMsgs = append(cachedMsgs, m)
		} else {
			cachedMsgs = append(cachedMsgs, msg)
		}
	}

	// Add cache_control to the last tool definition
	if len(tools) > 0 {
		cachedTools := make([]map[string]any, len(tools))
		copy(cachedTools, tools)
		last := copyMap(cachedTools[len(cachedTools)-1])
		// cache_control goes on the function spec, inside the tool
		if fn, ok := last["function"].(map[string]any); ok {
			fnCopy := copyMap(fn)
			fnCopy["cache_control"] = map[string]string{"type": "ephemeral"}
			last["function"] = fnCopy
		}
		cachedTools[len(cachedTools)-1] = last
		return cachedMsgs, cachedTools
	}

	return cachedMsgs, tools
}

// Chat sends messages to the LLM and returns a response.
func (p *Provider) Chat(messages []map[string]any, tools []map[string]any, maxTokens int, temperature float64) (*Response, error) {
	// Apply model-specific parameter overrides
	temperature = applyModelOverrides(p.Model, temperature)

	// Sanitize empty content to prevent provider 400 errors
	sanitized := sanitizeMessages(messages)

	// Apply prompt caching for supported providers
	finalMsgs, finalTools := sanitized, tools
	if p.supportsPromptCaching() {
		finalMsgs, finalTools = applyCacheControl(sanitized, tools)
	}

	body := map[string]any{
		"model":       p.Model,
		"messages":    finalMsgs,
		"max_tokens":  maxTokens,
		"temperature": temperature,
	}
	if len(finalTools) > 0 {
		body["tools"] = finalTools
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal error: %w", err)
	}

	req, err := http.NewRequest("POST", p.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("request error: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http error: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("LLM API error %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse error: %w\nbody: %s", err, string(respBody))
	}

	if len(result.Choices) == 0 {
		return &Response{Content: "(empty response from LLM)"}, nil
	}

	msg := result.Choices[0].Message
	response := &Response{Content: msg.Content}

	for _, tc := range msg.ToolCalls {
		var args map[string]any
		repaired := repairJSON(tc.Function.Arguments)
		if err := json.Unmarshal([]byte(repaired), &args); err != nil {
			log.Printf("[provider] JSON repair failed for tool %s: %v (raw: %s)", tc.Function.Name, err, tc.Function.Arguments)
			args = make(map[string]any)
		}
		response.ToolCalls = append(response.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	return response, nil
}

// ---------------------------------------------------------------------------
// Tolerant JSON repair (Item #8)
// ---------------------------------------------------------------------------

// repairJSON attempts to fix common LLM JSON errors before parsing.
// It handles: trailing commas, single quotes, unquoted property names,
// missing closing braces/brackets, and JavaScript-style comments.
func repairJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "{}"
	}

	// Remove JavaScript-style single-line comments: // ...
	singleLineComment := regexp.MustCompile(`//[^\n]*`)
	s = singleLineComment.ReplaceAllString(s, "")

	// Remove JavaScript-style multi-line comments: /* ... */
	multiLineComment := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = multiLineComment.ReplaceAllString(s, "")

	s = strings.TrimSpace(s)

	// Replace single-quoted strings with double-quoted strings.
	// This is done carefully to avoid breaking strings that contain quotes.
	s = replaceSingleQuotes(s)

	// Fix unquoted property names: { foo: "bar" } -> { "foo": "bar" }
	unquotedKey := regexp.MustCompile(`([{,]\s*)([a-zA-Z_][a-zA-Z0-9_]*)\s*:`)
	s = unquotedKey.ReplaceAllString(s, `$1"$2":`)

	// Remove trailing commas before closing braces/brackets: {a:1,} -> {a:1}
	trailingComma := regexp.MustCompile(`,\s*([}\]])`)
	s = trailingComma.ReplaceAllString(s, "$1")

	// Fix missing closing braces/brackets by counting open/close
	s = balanceBrackets(s)

	return s
}

// replaceSingleQuotes replaces single-quoted strings with double-quoted strings.
// It walks the string character by character to handle escapes correctly.
func replaceSingleQuotes(s string) string {
	var buf strings.Builder
	buf.Grow(len(s))

	inDouble := false
	inSingle := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		// Handle escape sequences inside strings
		if (inDouble || inSingle) && ch == '\\' && i+1 < len(s) {
			buf.WriteByte(ch)
			i++
			buf.WriteByte(s[i])
			continue
		}

		if ch == '"' && !inSingle {
			inDouble = !inDouble
			buf.WriteByte(ch)
			continue
		}

		if ch == '\'' && !inDouble {
			if !inSingle {
				inSingle = true
				buf.WriteByte('"')
			} else {
				inSingle = false
				buf.WriteByte('"')
			}
			continue
		}

		// Inside a single-quoted string, escape any literal double quotes
		if inSingle && ch == '"' {
			buf.WriteByte('\\')
		}

		buf.WriteByte(ch)
	}

	return buf.String()
}

// balanceBrackets appends missing closing braces/brackets.
func balanceBrackets(s string) string {
	var stack []byte
	inString := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		// Handle escape inside strings
		if inString && ch == '\\' && i+1 < len(s) {
			i++
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		switch ch {
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 && stack[len(stack)-1] == ch {
				stack = stack[:len(stack)-1]
			}
		}
	}

	// Append missing closers in reverse order
	var buf strings.Builder
	buf.WriteString(s)
	for i := len(stack) - 1; i >= 0; i-- {
		buf.WriteByte(stack[i])
	}
	return buf.String()
}

// sanitizeMessages replaces empty content that causes provider 400 errors.
// Empty content can appear when MCP tools return nothing or tool results are empty.
func sanitizeMessages(messages []map[string]any) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		content := msg["content"]
		role, _ := msg["role"].(string)

		switch c := content.(type) {
		case string:
			if c == "" {
				clean := copyMap(msg)
				// Assistant messages with tool_calls can have nil content
				if role == "assistant" && msg["tool_calls"] != nil {
					clean["content"] = ""
				} else {
					clean["content"] = "(empty)"
				}
				result = append(result, clean)
				continue
			}
		case nil:
			if role != "assistant" || msg["tool_calls"] == nil {
				clean := copyMap(msg)
				clean["content"] = "(empty)"
				result = append(result, clean)
				continue
			}
		}

		result = append(result, msg)
	}
	return result
}

func copyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
