package agent

import (
	"encoding/json"
	"testing"
)

// ── unwrapMcpEnvValue ──

func TestUnwrapMcpEnvValuePlainString(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`"/path/to/context"`)
	got := unwrapMcpEnvValue(raw)
	if got != "/path/to/context" {
		t.Errorf("got %q, want /path/to/context", got)
	}
}

func TestUnwrapMcpEnvValueWrappedPlain(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"plain","value":"/path/to/context"}`)
	got := unwrapMcpEnvValue(raw)
	if got != "/path/to/context" {
		t.Errorf("got %q, want /path/to/context", got)
	}
}

func TestUnwrapMcpEnvValueEmptyString(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`""`)
	got := unwrapMcpEnvValue(raw)
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestUnwrapMcpEnvValueWrappedEmptyValue(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"plain","value":""}`)
	got := unwrapMcpEnvValue(raw)
	if got != "" {
		t.Errorf("got %q, want empty (wrapped empty value is valid)", got)
	}
}

func TestUnwrapMcpEnvValueUnknownTypeReturnsEmpty(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"type":"secret","value":"/path"}`)
	got := unwrapMcpEnvValue(raw)
	if got != "" {
		t.Errorf("got %q, want empty for unknown type", got)
	}
}

func TestUnwrapMcpEnvValueNestedObjectReturnsEmpty(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"nested":"object"}`)
	got := unwrapMcpEnvValue(raw)
	if got != "" {
		t.Errorf("got %q, want empty for object without type:plain", got)
	}
}

func TestUnwrapMcpEnvValueNumberReturnsEmpty(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`42`)
	got := unwrapMcpEnvValue(raw)
	if got != "" {
		t.Errorf("got %q, want empty for number", got)
	}
}

// ── normalizeMcpConfigEnv ──

func TestNormalizeMcpConfigEnvNilReturnsNil(t *testing.T) {
	t.Parallel()
	got, err := normalizeMcpConfigEnv(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("got %s, want nil", got)
	}
}

func TestNormalizeMcpConfigEnvNullReturnsUnchanged(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage("null")
	got, err := normalizeMcpConfigEnv(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "null" {
		t.Errorf("got %s, want null", got)
	}
}

func TestNormalizeMcpConfigEnvNoMcpServersReturnsUnchanged(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`{"other":"field"}`)
	got, err := normalizeMcpConfigEnv(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No mcpServers field → original returned.
	if string(got) != string(raw) {
		t.Errorf("got %s, want %s", got, raw)
	}
}

func TestNormalizeMcpConfigEnvPlainStringsPassThrough(t *testing.T) {
	t.Parallel()
	// Env values already plain strings should be returned unchanged.
	raw := json.RawMessage(`{"mcpServers":{"loom":{"command":"loom","args":["mcp"],"env":{"LOOM_CONTEXT_DIR":"/home/art/.config/loom/art"}}}}`)
	got, err := normalizeMcpConfigEnv(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No change needed, should decode to the same env value.
	var parsed struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got := parsed.McpServers["loom"].Env["LOOM_CONTEXT_DIR"]; got != "/home/art/.config/loom/art" {
		t.Errorf("LOOM_CONTEXT_DIR: got %q, want /home/art/.config/loom/art", got)
	}
}

func TestNormalizeMcpConfigEnvUnwrapsWrappedValues(t *testing.T) {
	t.Parallel()
	// Wrapped {type:"plain",value:"..."} entries must be unwrapped.
	raw := json.RawMessage(`{"mcpServers":{"loom":{"command":"loom","args":["mcp"],"env":{"LOOM_CONTEXT_DIR":{"type":"plain","value":"/home/art/.config/loom/art"}}}}}`)
	got, err := normalizeMcpConfigEnv(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal normalized result: %v", err)
	}
	if v := parsed.McpServers["loom"].Env["LOOM_CONTEXT_DIR"]; v != "/home/art/.config/loom/art" {
		t.Errorf("LOOM_CONTEXT_DIR after unwrap: got %q, want /home/art/.config/loom/art", v)
	}
}

func TestNormalizeMcpConfigEnvMixedWrappedAndPlain(t *testing.T) {
	t.Parallel()
	// A server config with some wrapped and some plain env values.
	raw := json.RawMessage(`{"mcpServers":{"srv":{"command":"cmd","env":{"PLAIN":"already-plain","WRAPPED":{"type":"plain","value":"unwrapped-value"}}}}}`)
	got, err := normalizeMcpConfigEnv(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed struct {
		McpServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	srv := parsed.McpServers["srv"].Env
	if srv["PLAIN"] != "already-plain" {
		t.Errorf("PLAIN: got %q, want already-plain", srv["PLAIN"])
	}
	if srv["WRAPPED"] != "unwrapped-value" {
		t.Errorf("WRAPPED: got %q, want unwrapped-value", srv["WRAPPED"])
	}
}

func TestNormalizeMcpConfigEnvMalformedJSONReturnsError(t *testing.T) {
	t.Parallel()
	_, err := normalizeMcpConfigEnv(json.RawMessage(`not json`))
	if err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestNormalizeMcpConfigEnvPreservesNonEnvFields(t *testing.T) {
	t.Parallel()
	// Non-env fields in mcpServer entries (command, args, url, headers, type)
	// must survive the round-trip unchanged.
	raw := json.RawMessage(`{"mcpServers":{"srv":{"command":"mybin","args":["--flag"],"env":{"K":{"type":"plain","value":"v"}}}}}`)
	got, err := normalizeMcpConfigEnv(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var parsed struct {
		McpServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	srv := parsed.McpServers["srv"]
	if srv.Command != "mybin" {
		t.Errorf("command: got %q, want mybin", srv.Command)
	}
	if len(srv.Args) != 1 || srv.Args[0] != "--flag" {
		t.Errorf("args: got %v, want [--flag]", srv.Args)
	}
	if srv.Env["K"] != "v" {
		t.Errorf("env.K: got %q, want v", srv.Env["K"])
	}
}
