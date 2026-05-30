package daemon

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// stripClaudeSessionThinkingBlocks strips thinking/redacted_thinking content
// blocks from all prior assistant messages in a Claude CLI session JSONL file.
//
// Background: Anthropic cryptographically signs every thinking block it emits.
// On any continuation request the prior-turn thinking blocks must be sent back
// byte-for-byte with signature intact — or omitted entirely. Claude Code CLI
// re-serialises its stored session before sending to Anthropic, which can alter
// whitespace or field ordering within the blocks, invalidating the signature and
// causing a 400 "thinking or redacted_thinking blocks cannot be modified".
//
// Stripping the blocks entirely is the safe path: the API accepts history that
// contains no thinking blocks. Text and tool_use/tool_result blocks are
// preserved so the agent still has full task context across turns.
//
// The session file lives at:
//
//	~/.claude/projects/<encoded-cwd>/<session-id>.jsonl
//
// where <encoded-cwd> is the workdir path with every '/' replaced by '-'.
// If the file does not exist (session stored under a different cwd, or already
// GC'd), the function is a no-op — Claude CLI will handle a missing-session
// resume by starting fresh.
func stripClaudeSessionThinkingBlocks(workDir, sessionID string, logger *slog.Logger) {
	if workDir == "" || sessionID == "" {
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		logger.Debug("strip-claude-thinking: could not resolve home dir", "error", err)
		return
	}

	// Claude Code encodes the project directory by replacing '/' with '-'.
	// The leading '/' becomes a leading '-', so the directory name starts with '-'.
	encoded := strings.ReplaceAll(workDir, "/", "-")
	sessionFile := filepath.Join(home, ".claude", "projects", encoded, sessionID+".jsonl")

	data, err := os.ReadFile(sessionFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Debug("strip-claude-thinking: could not read session file", "path", sessionFile, "error", err)
		}
		return
	}

	lines := strings.Split(string(data), "\n")
	modified := make([]string, 0, len(lines))
	changed := false

	for _, line := range lines {
		stripped, wasChanged := stripThinkingFromSessionLine(line)
		if wasChanged {
			changed = true
		}
		modified = append(modified, stripped)
	}

	if !changed {
		return
	}

	if err := os.WriteFile(sessionFile, []byte(strings.Join(modified, "\n")), 0o644); err != nil {
		logger.Warn("strip-claude-thinking: could not write modified session file", "path", sessionFile, "error", err)
		return
	}

	logger.Info("strip-claude-thinking: stripped thinking blocks from session", "path", sessionFile)
}

// stripThinkingFromSessionLine strips thinking/redacted_thinking blocks from a
// single JSONL line representing an assistant message. Returns the (possibly
// modified) line and whether any block was stripped.
//
// All other entry types (user, queue-operation, last-prompt, …) are returned
// unchanged. Lines that fail JSON parsing are returned unchanged so the file
// is never left in a truncated state.
func stripThinkingFromSessionLine(line string) (string, bool) {
	if line == "" {
		return line, false
	}

	// Fast path: if neither thinking variant appears in the line, skip parsing.
	if !strings.Contains(line, `"thinking"`) && !strings.Contains(line, `"redacted_thinking"`) {
		return line, false
	}

	// Parse the outer entry. Use map[string]json.RawMessage so all fields are
	// preserved as raw bytes — only the parts we touch are re-serialised.
	var entry map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return line, false
	}

	typeRaw, ok := entry["type"]
	if !ok {
		return line, false
	}

	// Only assistant messages carry thinking blocks.
	var entryType string
	if err := json.Unmarshal(typeRaw, &entryType); err != nil {
		return line, false
	}
	if entryType != "assistant" {
		return line, false
	}

	msgRaw, ok := entry["message"]
	if !ok {
		return line, false
	}

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return line, false
	}

	contentRaw, ok := msg["content"]
	if !ok {
		return line, false
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return line, false
	}

	kept := make([]json.RawMessage, 0, len(blocks))
	stripped := false
	for _, block := range blocks {
		var t struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(block, &t); err != nil {
			kept = append(kept, block)
			continue
		}
		if t.Type == "thinking" || t.Type == "redacted_thinking" {
			stripped = true
			continue
		}
		kept = append(kept, block)
	}

	if !stripped {
		return line, false
	}

	// Don't produce an assistant message with zero content blocks — leave it
	// as-is so Claude Code can still interpret the turn boundary correctly.
	if len(kept) == 0 {
		return line, false
	}

	newContent, err := json.Marshal(kept)
	if err != nil {
		return line, false
	}
	msg["content"] = json.RawMessage(newContent)

	newMsg, err := json.Marshal(msg)
	if err != nil {
		return line, false
	}
	entry["message"] = json.RawMessage(newMsg)

	newLine, err := json.Marshal(entry)
	if err != nil {
		return line, false
	}

	return string(newLine), true
}
