package channels

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PlatoX-Type/monet-bot/bus"
)

type LarkChannel struct {
	bus       *bus.MessageBus
	appID     string
	appSecret string
	port      int
	workspace string
	allowFrom map[string]bool

	token    string
	tokenMu  sync.Mutex
	tokenExp time.Time

	// Per-chat trigger mode: true = continue (respond to all), false = @ mode (only @mentions)
	chatModes map[string]bool
	modesMu   sync.RWMutex
}

func NewLark(mb *bus.MessageBus, appID, appSecret string, allowFrom []string, port int, workspace string) *LarkChannel {
	af := make(map[string]bool)
	for _, id := range allowFrom {
		af[id] = true
	}
	if port == 0 {
		port = 9000
	}
	return &LarkChannel{
		bus:       mb,
		appID:     appID,
		appSecret: appSecret,
		port:      port,
		workspace: workspace,
		allowFrom: af,
		chatModes: make(map[string]bool),
	}
}

func (l *LarkChannel) Name() string { return "lark" }

func (l *LarkChannel) Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/lark/event", l.handleEvent)
	mux.HandleFunc("/lark/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok"}`))
	})

	addr := fmt.Sprintf("0.0.0.0:%d", l.port)
	log.Printf("[lark] listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("[lark] server error: %v", err)
	}
}

func (l *LarkChannel) isContinueMode(chatID string) bool {
	l.modesMu.RLock()
	defer l.modesMu.RUnlock()
	return l.chatModes[chatID]
}

func (l *LarkChannel) setMode(chatID string, continueMode bool) {
	l.modesMu.Lock()
	defer l.modesMu.Unlock()
	l.chatModes[chatID] = continueMode
}

func (l *LarkChannel) handleEvent(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	defer r.Body.Close()

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}

	// URL verification challenge
	if challenge, ok := payload["challenge"].(string); ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"challenge": challenge})
		return
	}

	event, _ := payload["event"].(map[string]any)
	if event == nil {
		w.Write([]byte(`{"ok":true}`))
		return
	}

	msg, _ := event["message"].(map[string]any)
	sender, _ := event["sender"].(map[string]any)
	if msg == nil {
		w.Write([]byte(`{"ok":true}`))
		return
	}

	msgType, _ := msg["message_type"].(string)
	msgID, _ := msg["message_id"].(string)
	contentStr, _ := msg["content"].(string)

	log.Printf("[lark] event: type=%s id=%s content=%s", msgType, msgID, contentStr)

	senderID, _ := sender["sender_id"].(map[string]any)
	userID, _ := senderID["open_id"].(string)
	chatID, _ := msg["chat_id"].(string)

	// ACL check
	if len(l.allowFrom) > 0 && !l.allowFrom[userID] {
		w.Write([]byte(`{"ok":true}`))
		return
	}

	// Parse mentions from the message (Lark provides these for all message types)
	// Each mention has: key ("@_user_1"), id.open_id ("ou_xxx"), name ("Alice")
	mentionMap := parseMentions(msg)

	// Parse content based on message type
	var text string
	var images []string
	wasMentioned := false

	switch msgType {
	case "text":
		var content map[string]string
		json.Unmarshal([]byte(contentStr), &content)
		rawText := strings.TrimSpace(content["text"])
		wasMentioned = strings.HasPrefix(rawText, "@")
		text = rawText
		if wasMentioned && strings.Contains(text, " ") {
			text = strings.TrimSpace(text[strings.Index(text, " ")+1:])
		}
		// Resolve @_user_N placeholders to real names
		text = resolveMentions(text, mentionMap)

	case "image":
		// Image-only message: download and forward
		var content map[string]string
		json.Unmarshal([]byte(contentStr), &content)
		imageKey := content["image_key"]
		if imageKey == "" {
			w.Write([]byte(`{"ok":true}`))
			return
		}
		path, err := l.downloadImage(msgID, imageKey)
		if err != nil {
			log.Printf("[lark] image download failed: %v", err)
			w.Write([]byte(`{"ok":true}`))
			return
		}
		images = append(images, path)
		text = "[User sent an image]"
		wasMentioned = true // always process image messages

	case "post":
		// Rich text — extract text and any inline images
		var post map[string]any
		json.Unmarshal([]byte(contentStr), &post)
		text, images, wasMentioned = l.parsePost(post, msgID)

	default:
		w.Write([]byte(`{"ok":true}`))
		return
	}

	if text == "" && len(images) == 0 {
		w.Write([]byte(`{"ok":true}`))
		return
	}

	// Handle mode switch commands (always respond regardless of mode)
	cmdLower := strings.ToLower(strings.TrimSpace(text))
	if cmdLower == "/continue" || cmdLower == "/monitor" {
		l.setMode(chatID, true)
		log.Printf("[lark] chat %s -> continue mode", chatID)
		l.bus.Outbound <- bus.OutboundMessage{
			Channel: "lark", ChatID: chatID,
			Text: "\xF0\x9F\x94\x84 Continue mode ON \xe2\x80\x94 I'll respond to all messages. Say /atmode to switch back.",
		}
		w.Write([]byte(`{"ok":true}`))
		return
	}
	if cmdLower == "/atmode" || cmdLower == "/quiet" {
		l.setMode(chatID, false)
		log.Printf("[lark] chat %s -> @ mode", chatID)
		l.bus.Outbound <- bus.OutboundMessage{
			Channel: "lark", ChatID: chatID,
			Text: "\xF0\x9F\x94\x95 @ mode ON \xe2\x80\x94 I'll only respond when @mentioned. Say /continue to switch back.",
		}
		w.Write([]byte(`{"ok":true}`))
		return
	}

	// Filter: in @ mode, only process if @mentioned
	if !l.isContinueMode(chatID) && !wasMentioned {
		w.Write([]byte(`{"ok":true}`))
		return
	}

	// Append image paths to text so the agent knows about them
	if len(images) > 0 {
		text += "\n\n\xf0\x9f\x93\x8e Attached images (saved to workspace):"
		for _, img := range images {
			text += "\n- " + img
		}
	}

	// Auto-learn team member Lark IDs from @mentions
	if len(mentionMap) > 0 {
		go autoLearnTeamMembers(l.workspace, mentionMap)
	}

	log.Printf("[lark] message from %s: %s (images: %d)", userID, text, len(images))

	l.bus.Inbound <- bus.InboundMessage{
		Channel:   "lark",
		ChatID:    chatID,
		User:      userID,
		Text:      text,
		Images:    images,
		Timestamp: time.Now(),
	}

	w.Write([]byte(`{"ok":true}`))
}

func (l *LarkChannel) Send(chatID, text string) {
	token := l.getTenantToken()
	if token == "" {
		log.Println("[lark] no tenant token")
		return
	}

	if len(text) > 25000 {
		text = text[:25000] + "\n\n... (truncated)"
	}

	// Decide format: status/short messages as plain text, everything else as card with markdown
	var msgType, content string
	if isStatusMessage(text) {
		msgType = "text"
		contentJSON, _ := json.Marshal(map[string]string{"text": text})
		content = string(contentJSON)
	} else {
		msgType = "interactive"
		content = buildLarkCard(text)
	}

	payload, _ := json.Marshal(map[string]any{
		"receive_id": chatID,
		"msg_type":   msgType,
		"content":    content,
	})

	req, _ := http.NewRequest("POST", "https://open.larksuite.com/open-apis/im/v1/messages?receive_id_type=chat_id", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[lark] send error: %v", err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("[lark] send failed: %d %s", resp.StatusCode, string(respBody))
	} else {
		// Check for Lark API-level errors (HTTP 200 but code != 0)
		var result struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(respBody, &result) == nil && result.Code != 0 {
			log.Printf("[lark] send error: code=%d msg=%s (type=%s)", result.Code, result.Msg, msgType)
		}
	}
}

// isStatusMessage returns true for short ephemeral messages (working hints, mode switches)
// that should stay as plain text instead of a card.
func isStatusMessage(text string) bool {
	if strings.HasPrefix(text, "\xe2\x9a\x99\xef\xb8\x8f [working]") {
		return true
	}
	if strings.HasPrefix(text, "\xf0\x9f\x94\x84 Continue mode") || strings.HasPrefix(text, "\xf0\x9f\x94\x95 @ mode") {
		return true
	}
	// Very short messages don't need a card
	if len(text) < 80 && !strings.Contains(text, "\n") && !strings.Contains(text, "**") && !strings.Contains(text, "```") {
		return true
	}
	return false
}

// buildLarkCard wraps markdown content in a Lark interactive card.
// Lark cards support: **bold**, *italic*, ~~strikethrough~~, `code`,
// [link](url), bullet lists (- item), code blocks (```lang\n...\n```).
// Headers (# ...) are not supported, so we convert them to bold text.
func buildLarkCard(text string) string {
	// Convert markdown headers to bold (Lark cards don't support #)
	md := convertHeadersToBold(text)

	// Split into chunks if content is very long (Lark card markdown element limit)
	elements := []map[string]any{}

	// Split on horizontal rules (---) to create visual sections
	sections := strings.Split(md, "\n---\n")
	for i, section := range sections {
		section = strings.TrimSpace(section)
		if section == "" {
			continue
		}
		if i > 0 {
			elements = append(elements, map[string]any{"tag": "hr"})
		}
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": section,
		})
	}

	if len(elements) == 0 {
		elements = append(elements, map[string]any{
			"tag":     "markdown",
			"content": md,
		})
	}

	card := map[string]any{
		"config": map[string]any{
			"wide_screen_mode": true,
		},
		"elements": elements,
	}

	cardJSON, _ := json.Marshal(card)
	return string(cardJSON)
}

// convertHeadersToBold turns "# Title" into "**Title**", "## Sub" into "**Sub**", etc.
func convertHeadersToBold(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") {
			lines[i] = "**" + strings.TrimPrefix(trimmed, "# ") + "**"
		} else if strings.HasPrefix(trimmed, "## ") {
			lines[i] = "**" + strings.TrimPrefix(trimmed, "## ") + "**"
		} else if strings.HasPrefix(trimmed, "### ") {
			lines[i] = "**" + strings.TrimPrefix(trimmed, "### ") + "**"
		}
	}
	return strings.Join(lines, "\n")
}

// downloadImage fetches an image from Lark and saves it to workspace/uploads/.
func (l *LarkChannel) downloadImage(msgID, imageKey string) (string, error) {
	token := l.getTenantToken()
	if token == "" {
		return "", fmt.Errorf("no tenant token")
	}

	url := fmt.Sprintf("https://open.larksuite.com/open-apis/im/v1/messages/%s/resources/%s?type=image", msgID, imageKey)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	// Determine extension from content-type
	ext := ".png"
	ct := resp.Header.Get("Content-Type")
	switch {
	case strings.Contains(ct, "jpeg"), strings.Contains(ct, "jpg"):
		ext = ".jpg"
	case strings.Contains(ct, "gif"):
		ext = ".gif"
	case strings.Contains(ct, "webp"):
		ext = ".webp"
	}

	// Save to workspace/uploads/
	uploadsDir := filepath.Join(l.workspace, "uploads")
	os.MkdirAll(uploadsDir, 0o755)

	filename := fmt.Sprintf("%d_%s%s", time.Now().UnixMilli(), imageKey[:min(8, len(imageKey))], ext)
	savePath := filepath.Join(uploadsDir, filename)

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read error: %w", err)
	}
	if err := os.WriteFile(savePath, data, 0o644); err != nil {
		return "", fmt.Errorf("write error: %w", err)
	}

	// Return workspace-relative path for the agent
	relPath := "uploads/" + filename
	log.Printf("[lark] saved image: %s (%d bytes)", relPath, len(data))
	return relPath, nil
}

// parsePost extracts text, @mentions, and images from a Lark rich-text (post) message.
// Returns: text content, image paths, whether bot was @mentioned.
func (l *LarkChannel) parsePost(post map[string]any, msgID string) (string, []string, bool) {
	var textParts []string
	var images []string
	mentioned := false

	// Post content can be at top level {"title","content"} or under a language key {"zh_cn":{"title","content"}}
	body := post
	if _, hasContent := body["content"]; !hasContent {
		for _, lang := range []string{"zh_cn", "en_us"} {
			if b, ok := post[lang].(map[string]any); ok {
				body = b
				break
			}
		}
	}

	if title, ok := body["title"].(string); ok && title != "" {
		textParts = append(textParts, title+"\n")
	}

	content, _ := body["content"].([]any)
	for _, paragraph := range content {
		elems, _ := paragraph.([]any)
		for _, elem := range elems {
			e, _ := elem.(map[string]any)
			if e == nil {
				continue
			}
			tag, _ := e["tag"].(string)
			switch tag {
			case "text":
				if t, ok := e["text"].(string); ok {
					textParts = append(textParts, t)
				}
			case "at":
				// @mention — include the user's name in text
				mentioned = true
				if userName, ok := e["user_name"].(string); ok && userName != "" {
					textParts = append(textParts, "@"+userName)
				}
			case "a":
				href, _ := e["href"].(string)
				t, _ := e["text"].(string)
				if t != "" {
					textParts = append(textParts, fmt.Sprintf("[%s](%s)", t, href))
				}
			case "img":
				imageKey, _ := e["image_key"].(string)
				if imageKey != "" {
					path, err := l.downloadImage(msgID, imageKey)
					if err != nil {
						log.Printf("[lark] post image download failed: %v", err)
					} else {
						images = append(images, path)
					}
				}
			}
		}
		textParts = append(textParts, "\n")
	}

	return strings.TrimSpace(strings.Join(textParts, "")), images, mentioned
}

func (l *LarkChannel) getTenantToken() string {
	l.tokenMu.Lock()
	defer l.tokenMu.Unlock()

	if l.token != "" && time.Now().Before(l.tokenExp) {
		return l.token
	}

	payload, _ := json.Marshal(map[string]string{
		"app_id":     l.appID,
		"app_secret": l.appSecret,
	})

	resp, err := http.Post(
		"https://open.larksuite.com/open-apis/auth/v3/tenant_access_token/internal",
		"application/json",
		bytes.NewReader(payload),
	)
	if err != nil {
		log.Printf("[lark] token error: %v", err)
		return ""
	}
	defer resp.Body.Close()

	var result struct {
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"`
	}
	json.NewDecoder(resp.Body).Decode(&result)

	l.token = result.Token
	l.tokenExp = time.Now().Add(time.Duration(result.Expire-300) * time.Second) // refresh 5 min early
	return l.token
}

// mentionInfo holds resolved @mention data from Lark events.
type mentionInfo struct {
	Key    string // "@_user_1"
	OpenID string // "ou_xxx"
	Name   string // "Alice Chen"
}

// parseMentions extracts the mentions array from a Lark message object.
// Lark provides: [{"key":"@_user_1","id":{"open_id":"ou_xxx"},"name":"Alice"}]
func parseMentions(msg map[string]any) []mentionInfo {
	mentions, ok := msg["mentions"].([]any)
	if !ok {
		return nil
	}
	var result []mentionInfo
	for _, m := range mentions {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		info := mentionInfo{
			Key:  stringVal(mm, "key"),
			Name: stringVal(mm, "name"),
		}
		if idObj, ok := mm["id"].(map[string]any); ok {
			info.OpenID = stringVal(idObj, "open_id")
		}
		if info.Key != "" {
			result = append(result, info)
		}
	}
	return result
}

// resolveMentions replaces @_user_N placeholders with real names.
// e.g., "@_user_1 is jiezhu" -> "@JieZhu (ou_abc) is jiezhu"
func resolveMentions(text string, mentions []mentionInfo) string {
	for _, m := range mentions {
		if m.Key == "" || m.Name == "" {
			continue
		}
		replacement := "@" + m.Name
		if m.OpenID != "" {
			replacement += " (lark:" + m.OpenID + ")"
		}
		text = strings.ReplaceAll(text, m.Key, replacement)
	}
	return text
}

func stringVal(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// autoLearnTeamMembers updates TEAM.md with newly discovered Lark user IDs.
// Uses fuzzy matching: "Sean Yang" matches "seanyang" by normalizing (lowercase, strip spaces).
func autoLearnTeamMembers(workspace string, mentions []mentionInfo) {
	teamPath := filepath.Join(workspace, "TEAM.md")

	data, err := os.ReadFile(teamPath)
	if err != nil {
		return
	}
	content := string(data)

	updated := false
	for _, m := range mentions {
		if m.OpenID == "" || m.Name == "" {
			continue
		}

		// Skip if this open_id is already in the file
		if strings.Contains(content, m.OpenID) {
			continue
		}

		// Build normalized variants of the mention name for fuzzy matching
		// "Sean Yang" → ["seanyang", "sean yang", "sean", "yang"]
		nameNorm := normalize(m.Name)
		nameNoSpace := strings.ReplaceAll(nameNorm, " ", "")

		// Try to find an existing team member entry with "lark: unknown"
		needle := "| lark: unknown"
		lines := strings.Split(content, "\n")
		found := false
		for i, line := range lines {
			if !strings.Contains(line, needle) {
				continue
			}
			lineNorm := normalize(line)
			// Match if: normalized name appears in line, OR name-without-spaces appears in line
			if strings.Contains(lineNorm, nameNorm) || strings.Contains(lineNorm, nameNoSpace) ||
				(nameNoSpace != "" && strings.Contains(strings.ReplaceAll(lineNorm, " ", ""), nameNoSpace)) {
				lines[i] = strings.Replace(line, needle, "| lark: "+m.OpenID, 1)
				found = true
				updated = true
				log.Printf("[lark] auto-learned Lark ID for %s: %s", m.Name, m.OpenID)
				break
			}
			// Also check if the next line has git aliases that match
			if i+1 < len(lines) {
				aliasLine := normalize(lines[i+1])
				if strings.Contains(aliasLine, "git aliases") &&
					(strings.Contains(aliasLine, nameNorm) || strings.Contains(strings.ReplaceAll(aliasLine, " ", ""), nameNoSpace)) {
					lines[i] = strings.Replace(lines[i], needle, "| lark: "+m.OpenID, 1)
					found = true
					updated = true
					log.Printf("[lark] auto-learned Lark ID for %s (via alias): %s", m.Name, m.OpenID)
					break
				}
			}
		}
		if found {
			content = strings.Join(lines, "\n")
			continue
		}

		// Not found — don't auto-add new entries (let the LLM handle merges
		// via edit_file when users explain the mapping, to avoid duplicates)
		log.Printf("[lark] unknown team member: %s (%s) — no matching entry found, LLM can merge via edit_file", m.Name, m.OpenID)
	}

	if updated {
		os.WriteFile(teamPath, []byte(content), 0o644)
	}
}

// normalize lowercases and trims a string for fuzzy matching.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
