package agent

import (
	"bytes"
	"encoding/json"
	"sort"
)

// unwrapMcpEnvValue decodes a single MCP server env entry from a JSON raw
// value. Plain JSON strings pass through unchanged. Values in the Claude Code
// settings wrapped format — {"type":"plain","value":"<string>"} — are
// unwrapped to their raw string. Any other format (unknown type, nested
// object, array, …) returns the empty string so callers can skip the entry.
func unwrapMcpEnvValue(raw json.RawMessage) string {
	// Fast path: plain JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Wrapped format: {"type":"plain","value":"..."}.
	var w struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &w); err == nil && w.Type == "plain" {
		return w.Value
	}
	return ""
}

// normalizeMcpConfigEnv normalizes an MCP config JSON so that any env values
// stored in Claude Code's extended settings format
// ({"type":"plain","value":"..."}) are unwrapped to plain JSON strings.
// This ensures the spawned MCP-server processes receive the raw env value
// rather than a JSON-object string.
//
// Returns the input unchanged when it is empty, null, or contains no
// mcpServers with wrapped env entries. Top-level JSON parse errors are
// returned to the caller; individual entry errors degrade gracefully.
func normalizeMcpConfigEnv(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return raw, nil
	}

	// Parse the outer object as a generic map so we can replace mcpServers
	// without disturbing any other top-level keys.
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &outer); err != nil {
		return nil, err
	}

	serversRaw, ok := outer["mcpServers"]
	if !ok || len(bytes.TrimSpace(serversRaw)) == 0 {
		return raw, nil
	}

	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil || len(servers) == 0 {
		return raw, nil
	}

	changed := false
	for name, serverRaw := range servers {
		normalized, didChange := normalizeServerEnvEntries(serverRaw)
		if didChange {
			servers[name] = normalized
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}

	newServersRaw, err := json.Marshal(servers)
	if err != nil {
		return raw, nil // fall back to original on unexpected marshal error
	}
	outer["mcpServers"] = newServersRaw

	result, err := json.Marshal(outer)
	if err != nil {
		return raw, nil
	}
	return result, nil
}

// normalizeServerEnvEntries rewrites the "env" field inside a single
// mcpServer JSON object, unwrapping any wrapped env values. Returns the
// rewritten object and true when any value changed; returns the original
// raw bytes and false when no change is needed or parsing fails.
func normalizeServerEnvEntries(serverRaw json.RawMessage) (json.RawMessage, bool) {
	var server map[string]json.RawMessage
	if err := json.Unmarshal(serverRaw, &server); err != nil {
		return serverRaw, false
	}

	envRaw, ok := server["env"]
	if !ok {
		return serverRaw, false
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal(envRaw, &env); err != nil || len(env) == 0 {
		return serverRaw, false
	}

	changed := false
	for k, v := range env {
		// Skip values that are already plain JSON strings.
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			continue
		}
		// Try to unwrap the value.
		if unwrapped := unwrapMcpEnvValue(v); unwrapped != "" {
			b, err := json.Marshal(unwrapped)
			if err == nil {
				env[k] = b
				changed = true
			}
		}
	}
	if !changed {
		return serverRaw, false
	}

	// Re-serialize the env map with keys in sorted order for determinism.
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	newEnvRaw, err := json.Marshal(env)
	if err != nil {
		return serverRaw, false
	}
	server["env"] = newEnvRaw

	result, err := json.Marshal(server)
	if err != nil {
		return serverRaw, false
	}
	return result, true
}
