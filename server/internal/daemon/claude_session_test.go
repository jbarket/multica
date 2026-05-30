package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStripThinkingFromSessionLine verifies the per-line stripping logic.
func TestStripThinkingFromSessionLine(t *testing.T) {
	t.Run("empty line unchanged", func(t *testing.T) {
		out, changed := stripThinkingFromSessionLine("")
		if changed || out != "" {
			t.Fatalf("expected empty/unchanged, got %q changed=%v", out, changed)
		}
	})

	t.Run("non-assistant entry unchanged", func(t *testing.T) {
		line := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`
		out, changed := stripThinkingFromSessionLine(line)
		if changed {
			t.Fatal("user entry should not be changed")
		}
		if out != line {
			t.Fatalf("content altered: %q", out)
		}
	})

	t.Run("assistant with no thinking unchanged", func(t *testing.T) {
		line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`
		out, changed := stripThinkingFromSessionLine(line)
		if changed {
			t.Fatal("expected no change when no thinking blocks")
		}
		if out != line {
			t.Fatalf("content altered: %q", out)
		}
	})

	t.Run("strips thinking block keeps text", func(t *testing.T) {
		line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"secret","signature":"sig"},{"type":"text","text":"visible"}]}}`
		out, changed := stripThinkingFromSessionLine(line)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if strings.Contains(out, `"thinking"`) && strings.Contains(out, `"secret"`) {
			t.Fatal("thinking content still present after strip")
		}
		if !strings.Contains(out, `"visible"`) {
			t.Fatal("text block was incorrectly removed")
		}
	})

	t.Run("strips redacted_thinking block keeps tool_use", func(t *testing.T) {
		line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"redacted_thinking","data":"abc"},{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`
		out, changed := stripThinkingFromSessionLine(line)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if strings.Contains(out, `"redacted_thinking"`) {
			t.Fatal("redacted_thinking still present after strip")
		}
		if !strings.Contains(out, `"tool_use"`) {
			t.Fatal("tool_use block was incorrectly removed")
		}
	})

	t.Run("all-thinking content returns original to preserve turn boundary", func(t *testing.T) {
		// An assistant message whose ONLY block is thinking must be left intact
		// so Claude Code can still reconstruct the conversation tree.
		line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"only","signature":"sig"}]}}`
		out, changed := stripThinkingFromSessionLine(line)
		if changed {
			t.Fatal("should not change when kept would be empty")
		}
		if out != line {
			t.Fatalf("line altered: %q", out)
		}
	})

	t.Run("invalid JSON returned unchanged", func(t *testing.T) {
		line := `{invalid json`
		out, changed := stripThinkingFromSessionLine(line)
		if changed {
			t.Fatal("invalid JSON should not be changed")
		}
		if out != line {
			t.Fatalf("invalid JSON line was altered")
		}
	})
}

// TestStripClaudeSessionThinkingBlocks exercises the file-level path.
func TestStripClaudeSessionThinkingBlocks(t *testing.T) {
	// Redirect HOME so the function writes under a temp dir.
	tmpHome := t.TempDir()
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmpHome)
	defer func() { os.Setenv("HOME", origHome) }()

	// Build a fake workdir and session ID.
	fakeWorkDir := "/home/testuser/multica_workspaces/ws1/task1/workdir"
	sessionID := "test-session-abc123"

	// Compute the Claude project directory path.
	encoded := strings.ReplaceAll(fakeWorkDir, "/", "-")
	projectDir := filepath.Join(tmpHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")

	// Write a session file with thinking blocks mixed with text.
	content := strings.Join([]string{
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do something"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"internal","signature":"sig1"},{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"last-prompt","lastPrompt":"do something","leafUuid":"x","sessionId":"y"}`,
		``,
	}, "\n")
	if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := testNoopLogger(t)
	stripClaudeSessionThinkingBlocks(fakeWorkDir, sessionID, logger)

	got, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	result := string(got)

	if strings.Contains(result, `"thinking"`) && strings.Contains(result, `"internal"`) {
		t.Error("thinking content still present after strip")
	}
	if !strings.Contains(result, `"tool_use"`) {
		t.Error("tool_use block was incorrectly removed")
	}
	if !strings.Contains(result, `"done"`) {
		t.Error("text block was incorrectly removed")
	}
	if !strings.Contains(result, `"last-prompt"`) {
		t.Error("non-assistant entry was incorrectly removed")
	}
}

func TestStripClaudeSessionThinkingBlocks_NoFile(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	logger := testNoopLogger(t)
	// Should be a no-op without panicking when the session file doesn't exist.
	stripClaudeSessionThinkingBlocks("/some/workdir", "nonexistent-session", logger)
}

func TestStripClaudeSessionThinkingBlocks_NoThinking(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	fakeWorkDir := "/home/testuser/workdir"
	sessionID := "clean-session"
	encoded := strings.ReplaceAll(fakeWorkDir, "/", "-")
	projectDir := filepath.Join(tmpHome, ".claude", "projects", encoded)
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(projectDir, sessionID+".jsonl")
	original := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	statBefore, _ := os.Stat(sessionFile)
	logger := testNoopLogger(t)
	stripClaudeSessionThinkingBlocks(fakeWorkDir, sessionID, logger)
	statAfter, _ := os.Stat(sessionFile)

	// File must not be rewritten when nothing changed.
	if statBefore.ModTime() != statAfter.ModTime() {
		t.Error("file was rewritten despite no thinking blocks")
	}
}

// testNoopLogger returns a slog.Logger that discards all output.
func testNoopLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
