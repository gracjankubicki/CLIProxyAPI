package accountmodels

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/MoeForge/moego/json"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// verifyToolLoop uses a pinned executor/auth snapshot; it neither runs model-generated
// code nor touches the user's files. Full client apply_patch/MCP testing runs on sync.
func (m *Manager) verifyToolLoop(ctx context.Context, a *coreauth.Auth, model Model) error {
	exec, ok := m.auth.Executor(a.Provider)
	if !ok {
		return fmt.Errorf("probe executor unavailable")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("probe nonce unavailable")
	}
	nonce := hex.EncodeToString(random[:])
	user := map[string]any{"role": "user", "content": "Call proxy_sync_check with nonce " + nonce + ". After its result, reply with the exact returned text."}
	body := map[string]any{
		"model": model.ID, "stream": true, "store": false,
		"instructions":      "This is a bounded protocol integration test. Follow the user's tool request exactly.",
		"input":             []any{user},
		"tools":             []any{map[string]any{"type": "function", "name": "proxy_sync_check", "description": "Returns the verification result", "parameters": map[string]any{"type": "object", "properties": map[string]any{"nonce": map[string]any{"type": "string"}}, "required": []string{"nonce"}, "additionalProperties": false}, "strict": true}},
		"tool_choice":       map[string]any{"type": "function", "name": "proxy_sync_check"},
		"max_output_tokens": 256,
	}
	if len(model.Parameters.ReasoningLevels) > 0 {
		body["reasoning"] = map[string]any{"effort": model.Parameters.ReasoningLevels[0]}
	}
	response, err := probeResponse(ctx, exec, a, model.ID, body)
	if err != nil {
		return err
	}
	var callID string
	var output []map[string]any
	if json.Unmarshal(response["output"], &output) != nil {
		return fmt.Errorf("tool call missing")
	}
	for _, item := range output {
		if item["type"] != "function_call" || item["name"] != "proxy_sync_check" {
			continue
		}
		var args map[string]string
		arguments, _ := item["arguments"].(string)
		if json.Unmarshal([]byte(arguments), &args) != nil || args["nonce"] != nonce {
			continue
		}
		callID, _ = item["call_id"].(string)
	}
	if callID == "" {
		return fmt.Errorf("tool call missing")
	}
	input := []any{user}
	for _, item := range output {
		input = append(input, item)
	}
	marker := "PROXY_SYNC_OK_" + nonce
	input = append(input, map[string]any{"type": "function_call_output", "call_id": callID, "output": marker})
	body["input"] = input
	body["tool_choice"] = "none"
	response, err = probeResponse(ctx, exec, a, model.ID, body)
	if err != nil {
		return err
	}
	var final []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(response["output"], &final) != nil {
		return fmt.Errorf("tool continuation failed")
	}
	for _, item := range final {
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Type == "output_text" && strings.TrimSpace(part.Text) == marker {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("tool continuation failed")
}

func probeResponse(ctx context.Context, exec coreauth.ProviderExecutor, a *coreauth.Auth, model string, body map[string]any) (map[string]json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("probe request invalid")
	}
	format := translator.FormatOpenAIResponse
	stream, err := exec.ExecuteStream(ctx, a, executor.Request{Model: model, Payload: raw, Format: format}, executor.Options{SourceFormat: format, Stream: true, OriginalRequest: raw})
	if err != nil {
		if status, ok := err.(executor.StatusError); ok {
			return nil, fmt.Errorf("probe HTTP %d", status.StatusCode())
		}
		return nil, fmt.Errorf("probe request failed")
	}
	var all bytes.Buffer
	for chunk := range stream.Chunks {
		if chunk.Err != nil || chunk.Incomplete {
			return nil, fmt.Errorf("probe stream failed")
		}
		if all.Len()+len(chunk.Payload) > 4<<20 {
			return nil, fmt.Errorf("probe response too large")
		}
		all.Write(chunk.Payload)
		// Executors emit complete translated events, sometimes without SSE framing.
		all.WriteByte('\n')
	}
	for _, line := range bytes.Split(all.Bytes(), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		var event struct {
			Type     string                     `json:"type"`
			Response map[string]json.RawMessage `json:"response"`
		}
		if json.Unmarshal(line, &event) == nil && event.Type == "response.completed" {
			return event.Response, nil
		}
	}
	return nil, fmt.Errorf("probe stream incomplete")
}
