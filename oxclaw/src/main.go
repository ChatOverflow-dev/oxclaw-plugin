package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// maxChunkSize is the Teams message size limit. Replies longer than this
// are split into multiple sequential respond calls.
const maxChunkSize = 28000

// oxclaw-channel is an MCP server (Claude Code channel plugin) that bridges
// Claude Code ↔ oxclaw. It reads credentials from ~/.claude/channels/oxclaw/.env,
// polls for messages, filters by access.json, and delivers as MCP notifications.
//
// Config files:
//   ~/.claude/channels/oxclaw/.env         — OXCLAW_URL, OXCLAW_CLIENT_ID, OXCLAW_API_KEY
//   ~/.claude/channels/oxclaw/access.json  — allowlist policy

const stateDir = ".claude/channels/oxclaw"

func main() {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, stateDir)

	// Load credentials from .env file (set by /oxclaw:configure skill)
	loadDotEnv(filepath.Join(dir, ".env"))

	// Env vars can also be set directly (e.g., from .mcp.json or Docker)
	oxclawURL := envRequired("OXCLAW_URL")
	clientID := envRequired("OXCLAW_CLIENT_ID")
	apiKey := envRequired("OXCLAW_API_KEY")

	client := &oxclawClient{
		baseURL:  strings.TrimRight(oxclawURL, "/"),
		clientID: clientID,
		apiKey:   apiKey,
		http:     &http.Client{Timeout: 45 * time.Second},
	}

	accessFile := filepath.Join(dir, "access.json")

	s := server.NewMCPServer(
		"oxclaw",
		"1.0.0",
		server.WithToolCapabilities(false),
		server.WithExperimental(map[string]any{
			"claude/channel":            map[string]any{},
			"claude/channel/permission": map[string]any{},
		}),
		server.WithInstructions(strings.Join([]string{
			"The sender reads Teams or Outlook, not this session. Anything you want them to see must go through the reply tool — your transcript output never reaches their chat.",
			"Messages arrive as <channel source=\"oxclaw\" chat_id=\"...\" message_id=\"...\" user=\"...\" ts=\"...\">. Reply with the reply tool — pass chat_id back.",
			"reply accepts a body string and optional files (array of absolute paths). The response is delivered back through the original channel (Teams or Outlook) by the oxclaw server.",
			"You can also proactively message users with the send tool — provide a username and the message will be delivered via their preferred channel.",
			"Use react to add an emoji reaction to a message, and edit_message to update a previously sent message.",
			"Permission requests from Claude Code are forwarded to the user via the channel. Their response is relayed back.",
		}, "\n")),
	)

	s.AddTool(replyTool(), replyHandler(client))
	s.AddTool(sendTool(), sendHandler(client))
	s.AddTool(reactTool(), reactHandler())
	s.AddTool(editMessageTool(), editMessageHandler())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		cancel()
	}()

	go pollMessages(ctx, client, s, accessFile)

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintf(os.Stderr, "oxclaw-channel: %v\n", err)
		os.Exit(1)
	}
}

// --- Access control ---

type accessConfig struct {
	Policy      string   `json:"policy"`      // "allowlist", "domain", "open", "disabled"
	AllowFrom   []string `json:"allowFrom"`   // email addresses
	AckReaction string   `json:"ackReaction"` // emoji to react with on message delivery (e.g. "eyes"), empty = no ack
	SSO         struct {
		Provider string `json:"provider"`
		Domain   string `json:"domain"`
	} `json:"sso"`
}

func loadAccess(path string) *accessConfig {
	data, err := os.ReadFile(path)
	if err != nil {
		return &accessConfig{Policy: "allowlist"}
	}
	var ac accessConfig
	if err := json.Unmarshal(data, &ac); err != nil {
		return &accessConfig{Policy: "allowlist"}
	}
	if ac.Policy == "" {
		ac.Policy = "allowlist"
	}
	return &ac
}

func (ac *accessConfig) isAllowed(senderEmail string) bool {
	switch ac.Policy {
	case "disabled":
		return false
	case "open":
		return true
	case "domain":
		if ac.SSO.Domain == "" {
			return false
		}
		parts := strings.SplitN(senderEmail, "@", 2)
		return len(parts) == 2 && strings.EqualFold(parts[1], ac.SSO.Domain)
	default: // "allowlist"
		lower := strings.ToLower(senderEmail)
		for _, a := range ac.AllowFrom {
			if strings.ToLower(a) == lower {
				return true
			}
		}
		return false
	}
}

// --- .env loader ---

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		// Don't override existing env vars
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}

// --- oxclaw HTTP client ---

type oxclawClient struct {
	baseURL  string
	clientID string
	apiKey   string
	http     *http.Client
}

type inboundMessage struct {
	MessageID   string `json:"message_id"`
	Source      string `json:"source"`
	SenderEmail string `json:"sender_email"`
	SenderName  string `json:"sender_name"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
	ThreadID    string `json:"thread_id"`
	Timestamp   string `json:"timestamp"`
}

func (c *oxclawClient) pollMessage(ctx context.Context) (*inboundMessage, error) {
	url := fmt.Sprintf("%s/api/v1/clients/%s/messages", c.baseURL, c.clientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("poll failed (%d): %s", resp.StatusCode, string(body))
	}

	var msg inboundMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, fmt.Errorf("decoding message: %w", err)
	}
	return &msg, nil
}

func (c *oxclawClient) respond(ctx context.Context, messageID, body string) error {
	url := fmt.Sprintf("%s/api/v1/clients/%s/respond", c.baseURL, c.clientID)
	payload, _ := json.Marshal(map[string]string{
		"message_id": messageID,
		"body":       body,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("respond failed (%d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *oxclawClient) send(ctx context.Context, username, subject, body string) error {
	url := fmt.Sprintf("%s/api/v1/clients/%s/send", c.baseURL, c.clientID)
	payload, _ := json.Marshal(map[string]string{
		"username": username,
		"subject":  subject,
		"body":     body,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send failed (%d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// --- Message polling loop ---

func pollMessages(ctx context.Context, client *oxclawClient, s *server.MCPServer, accessFile string) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msg, err := client.pollMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "oxclaw-channel: poll error: %v\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		if msg == nil {
			continue
		}

		// Access control — re-read on every message (like Telegram channel does)
		ac := loadAccess(accessFile)
		if !ac.isAllowed(msg.SenderEmail) {
			fmt.Fprintf(os.Stderr, "oxclaw-channel: dropped message from %s (policy: %s)\n", msg.SenderEmail, ac.Policy)
			continue
		}

		content := msg.Body
		if msg.Subject != "" && msg.Source == "outlook" {
			content = fmt.Sprintf("[Subject: %s]\n\n%s", msg.Subject, msg.Body)
		}

		// Check if this is a permission response (subject or body contains permission grant/deny)
		if isPermissionResponse(msg) {
			s.SendNotificationToAllClients(
				"notifications/claude/channel/permission_response",
				map[string]any{
					"decision": parsePermissionDecision(msg.Body),
					"meta": map[string]any{
						"chat_id":    msg.MessageID,
						"message_id": msg.MessageID,
						"user":       msg.SenderName,
						"user_id":    msg.SenderEmail,
						"ts":         msg.Timestamp,
					},
				},
			)
			continue
		}

		s.SendNotificationToAllClients(
			"notifications/claude/channel",
			map[string]any{
				"content": content,
				"meta": map[string]any{
					"chat_id":    msg.MessageID,
					"message_id": msg.MessageID,
					"user":       msg.SenderName,
					"user_id":    msg.SenderEmail,
					"ts":         msg.Timestamp,
					"source":     msg.Source,
					"subject":    msg.Subject,
					"thread_id":  msg.ThreadID,
				},
			},
		)

		// Send ack reaction if configured
		if ac.AckReaction != "" {
			fmt.Fprintf(os.Stderr, "oxclaw-channel: ack reaction '%s' for message %s (not yet implemented via Graph API)\n", ac.AckReaction, msg.MessageID)
		}
	}
}

// --- MCP Tools ---

func replyTool() mcp.Tool {
	return mcp.NewTool("reply",
		mcp.WithDescription("Reply to a message received via oxclaw (Teams or Outlook). Pass the chat_id from the inbound <channel> block."),
		mcp.WithString("chat_id",
			mcp.Required(),
			mcp.Description("The message_id/chat_id from the inbound channel message."),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("The reply text to send back to the user."),
		),
		mcp.WithArray("files",
			mcp.Description("Optional array of absolute file paths to attach. Files are noted in the reply text as '[Attached: filename]'."),
			mcp.WithStringItems(),
		),
	)
}

func replyHandler(client *oxclawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		chatID := req.GetString("chat_id", "")
		text := req.GetString("text", "")
		if chatID == "" || text == "" {
			return mcp.NewToolResultError("chat_id and text are required"), nil
		}

		// Append file attachment notes if provided
		args := req.GetArguments()
		if filesRaw, ok := args["files"]; ok {
			if filesArr, ok := filesRaw.([]any); ok && len(filesArr) > 0 {
				var attachNotes []string
				for _, f := range filesArr {
					if s, ok := f.(string); ok && s != "" {
						base := filepath.Base(s)
						attachNotes = append(attachNotes, fmt.Sprintf("[Attached: %s]", base))
					}
				}
				if len(attachNotes) > 0 {
					text = text + "\n\n" + strings.Join(attachNotes, "\n")
				}
			}
		}

		// Split into chunks if text exceeds Teams limit
		chunks := chunkText(text, maxChunkSize)
		for _, chunk := range chunks {
			if err := client.respond(ctx, chatID, chunk); err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("reply failed: %v", err)), nil
			}
		}
		return mcp.NewToolResultText("sent"), nil
	}
}

// chunkText splits text into pieces of at most maxLen characters,
// breaking at newline boundaries when possible.
func chunkText(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}
		// Try to break at last newline within limit
		cut := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n"); idx > 0 {
			cut = idx + 1
		}
		chunks = append(chunks, text[:cut])
		text = text[cut:]
	}
	return chunks
}

func sendTool() mcp.Tool {
	return mcp.NewTool("send",
		mcp.WithDescription("Proactively send a message to a user via their preferred channel (Teams or Outlook). The user must have this client assigned."),
		mcp.WithString("username",
			mcp.Required(),
			mcp.Description("The username of the recipient."),
		),
		mcp.WithString("subject",
			mcp.Description("Email subject (Outlook). Defaults to 'Message from <client>'."),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("The message body to send."),
		),
	)
}

func sendHandler(client *oxclawClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		username := req.GetString("username", "")
		text := req.GetString("text", "")
		subject := req.GetString("subject", "")
		if username == "" || text == "" {
			return mcp.NewToolResultError("username and text are required"), nil
		}
		if err := client.send(ctx, username, subject, text); err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("send failed: %v", err)), nil
		}
		return mcp.NewToolResultText("sent"), nil
	}
}

// --- Permission request/response handling ---

// isPermissionResponse checks if an inbound message looks like a response
// to a permission request that was forwarded to the user.
func isPermissionResponse(msg *inboundMessage) bool {
	lower := strings.ToLower(msg.Body)
	subjectLower := strings.ToLower(msg.Subject)
	if strings.Contains(subjectLower, "permission") {
		return true
	}
	// Simple heuristic: if the body is just an approval/denial word
	trimmed := strings.TrimSpace(lower)
	return trimmed == "approve" || trimmed == "approved" ||
		trimmed == "deny" || trimmed == "denied" ||
		trimmed == "yes" || trimmed == "no" ||
		trimmed == "allow" || trimmed == "reject"
}

// parsePermissionDecision extracts approve/deny from a message body.
func parsePermissionDecision(body string) string {
	lower := strings.TrimSpace(strings.ToLower(body))
	switch lower {
	case "approve", "approved", "yes", "allow":
		return "approve"
	case "deny", "denied", "no", "reject":
		return "deny"
	default:
		// If longer text, look for keywords
		if strings.Contains(lower, "approve") || strings.Contains(lower, "allow") || strings.Contains(lower, "yes") {
			return "approve"
		}
		return "deny"
	}
}

// forwardPermissionRequest sends a permission request to the user via oxclaw.
func forwardPermissionRequest(ctx context.Context, client *oxclawClient, description string) {
	body := fmt.Sprintf("Claude Code is requesting permission:\n\n%s\n\nReply 'approve' or 'deny'.", description)
	// Use the send endpoint to reach the user; username is empty which means
	// the server should route to the client owner.
	if err := client.send(ctx, "", "Permission Request from Claude Code", body); err != nil {
		fmt.Fprintf(os.Stderr, "oxclaw-channel: failed to forward permission request: %v\n", err)
	}
}

// --- Additional MCP Tools ---

func reactTool() mcp.Tool {
	return mcp.NewTool("react",
		mcp.WithDescription("Add an emoji reaction to a message. Currently a no-op for Teams/Outlook (Graph API reactions not yet implemented)."),
		mcp.WithString("chat_id",
			mcp.Required(),
			mcp.Description("The message_id/chat_id to react to."),
		),
		mcp.WithString("emoji",
			mcp.Required(),
			mcp.Description("The emoji to react with (e.g. 'thumbsup', 'eyes', 'check')."),
		),
	)
}

func reactHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		chatID := req.GetString("chat_id", "")
		emoji := req.GetString("emoji", "")
		if chatID == "" || emoji == "" {
			return mcp.NewToolResultError("chat_id and emoji are required"), nil
		}
		// TODO: Implement via Graph API for Teams. Outlook does not support reactions.
		return mcp.NewToolResultText("reacted"), nil
	}
}

func editMessageTool() mcp.Tool {
	return mcp.NewTool("edit_message",
		mcp.WithDescription("Edit a previously sent message. Not yet supported for Outlook. Teams support planned via Graph API."),
		mcp.WithString("chat_id",
			mcp.Required(),
			mcp.Description("The message_id/chat_id of the message to edit."),
		),
		mcp.WithString("text",
			mcp.Required(),
			mcp.Description("The new text to replace the message content with."),
		),
	)
}

func editMessageHandler() server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		chatID := req.GetString("chat_id", "")
		text := req.GetString("text", "")
		if chatID == "" || text == "" {
			return mcp.NewToolResultError("chat_id and text are required"), nil
		}
		// TODO: Implement via Graph API for Teams. Not possible for Outlook emails.
		return mcp.NewToolResultError("edit_message is not yet supported for Teams/Outlook"), nil
	}
}

func envRequired(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "oxclaw-channel: required env var %s not set\n", key)
		fmt.Fprintf(os.Stderr, "oxclaw-channel: run /oxclaw:configure to set up credentials\n")
		os.Exit(1)
	}
	return v
}
