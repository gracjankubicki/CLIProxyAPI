package executor

import (
	"context"
	"fmt"
	"github.com/MoeForge/moego/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenAICompatExecutorKimiOpenRouterToolLinks(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var receivedBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedBody, _ = io.ReadAll(r.Body)
				var missingNames []string
				messages := ojson.GetBytes(receivedBody, "messages")
				messages.ForEach(func(_, message ojson.Result) bool {
					if message.Get("role").String() == "tool" && strings.TrimSpace(message.Get("name").String()) == "" {
						missingNames = append(missingNames, message.Get("tool_call_id").String())
					}
					return true
				})
				if len(missingNames) > 0 {
					http.Error(w, "Kimi K3 tool messages need a resolvable tool name: missing name for "+strings.Join(missingNames, ","), http.StatusBadRequest)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-kimi\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, `{"id":"chatcmpl-kimi","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor("openai-compatible-openrouter", &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{{
					Name:    "OpenRouter",
					BaseURL: server.URL,
					Models:  []config.OpenAICompatibilityModel{{Name: "moonshotai/kimi-k3", Alias: "or-kimi-k3"}},
				}},
			})
			auth := &cliproxyauth.Auth{
				Provider: "openai-compatible-openrouter",
				Attributes: map[string]string{
					"base_url":     server.URL,
					"api_key":      "test-key",
					"compat_name":  "OpenRouter",
					"provider_key": "openai-compatible-openrouter",
				},
			}
			req := cliproxyexecutor.Request{
				Model:   "or-kimi-k3",
				Payload: kimiOpenRouterRepeatedToolHistory(),
			}
			opts := cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAI,
				Stream:         stream,
			}

			if stream {
				result, errExecute := executor.ExecuteStream(context.Background(), auth, req, opts)
				if errExecute != nil {
					t.Fatalf("ExecuteStream error: %v; upstream body: %s", errExecute, receivedBody)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error: %v; upstream body: %s", chunk.Err, receivedBody)
					}
				}
			} else if _, errExecute := executor.Execute(context.Background(), auth, req, opts); errExecute != nil {
				t.Fatalf("Execute error: %v; upstream body: %s", errExecute, receivedBody)
			}

			assertKimiToolMessageNamesAndIDs(t, receivedBody)
		})
	}
}

func TestOpenAICompatExecutorKimiOpenRouterToolLinksScope(t *testing.T) {
	tests := []struct {
		name             string
		executorProvider string
		compatName       string
		model            string
		configuredName   string
		configuredAlias  string
	}{
		{
			name:             "non kimi OpenRouter model",
			executorProvider: "openai-compatible-openrouter",
			compatName:       "OpenRouter",
			model:            "or-glm-5.3",
			configuredName:   "z-ai/glm-5.3",
			configuredAlias:  "or-glm-5.3",
		},
		{
			name:             "Kimi model on another compatibility provider",
			executorProvider: "openai-compatible-kimi",
			compatName:       "Kimi",
			model:            "kimi-k3",
			configuredName:   "moonshotai/kimi-k3",
			configuredAlias:  "kimi-k3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var receivedBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl-scope","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()

			executor := NewOpenAICompatExecutor(tt.executorProvider, &config.Config{
				OpenAICompatibility: []config.OpenAICompatibility{{
					Name:    tt.compatName,
					BaseURL: server.URL,
					Models:  []config.OpenAICompatibilityModel{{Name: tt.configuredName, Alias: tt.configuredAlias}},
				}},
			})
			auth := &cliproxyauth.Auth{
				Provider: tt.executorProvider,
				Attributes: map[string]string{
					"base_url":     server.URL,
					"api_key":      "test-key",
					"compat_name":  tt.compatName,
					"provider_key": tt.executorProvider,
				},
			}
			_, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   tt.model,
				Payload: openRouterRepeatedToolHistory(tt.model),
			}, cliproxyexecutor.Options{
				SourceFormat:   sdktranslator.FormatOpenAIResponse,
				ResponseFormat: sdktranslator.FormatOpenAI,
			})
			if errExecute != nil {
				t.Fatalf("Execute error: %v; upstream body: %s", errExecute, receivedBody)
			}

			ojson.GetBytes(receivedBody, "messages").ForEach(func(_, message ojson.Result) bool {
				if message.Get("role").String() != "tool" {
					return true
				}
				gotName := strings.TrimSpace(message.Get("name").String())
				if gotName != "" {
					t.Fatalf("tool name = %q, want absent; body=%s", gotName, receivedBody)
				}
				return true
			})
		})
	}
}

func TestNormalizeOpenRouterKimiToolMessageLinksUsesOrderWithoutGuessingUnknownIDs(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call-a","type":"function","function":{"name":"exec_command","arguments":"{}"}},
				{"id":"call-b","type":"function","function":{"name":"read_file","arguments":"{}"}},
				{"id":"call-c","type":"function","function":{"name":"list_directory","arguments":"{}"}}
			]},
			{"role":"tool","name":"caller_name","content":"first"},
			{"role":"tool","content":"second"},
			{"role":"tool","tool_call_id":"call-c","content":"third"},
			{"role":"tool","tool_call_id":"unknown","content":"orphan"},
			{"role":"assistant","tool_calls":[
				{"id":"call-d","type":"function","function":{"name":"generated_name","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call-d","name":"existing_name","content":"existing"}
		]
	}`)

	got, errNormalize := normalizeOpenRouterKimiToolMessageLinks(body)
	if errNormalize != nil {
		t.Fatalf("normalizeOpenRouterKimiToolMessageLinks error: %v", errNormalize)
	}
	if name := ojson.GetBytes(got, "messages.1.name").String(); name != "caller_name" {
		t.Fatalf("existing ordered tool name = %q, want caller_name; body=%s", name, got)
	}
	if name := ojson.GetBytes(got, "messages.2.name").String(); name != "read_file" {
		t.Fatalf("next ordered tool name = %q, want read_file; body=%s", name, got)
	}
	if name := ojson.GetBytes(got, "messages.3.name").String(); name != "list_directory" {
		t.Fatalf("matched tool name = %q, want list_directory; body=%s", name, got)
	}
	if ojson.GetBytes(got, "messages.4.name").Exists() {
		t.Fatalf("unknown tool id received a guessed name; body=%s", got)
	}
	if name := ojson.GetBytes(got, "messages.6.name").String(); name != "existing_name" {
		t.Fatalf("existing tool name = %q, want existing_name; body=%s", name, got)
	}
}

func kimiOpenRouterRepeatedToolHistory() []byte {
	return openRouterRepeatedToolHistory("or-kimi-k3")
}

func openRouterRepeatedToolHistory(model string) []byte {
	items := make([]map[string]any, 0, 50)
	for turn := 0; turn < 10; turn++ {
		if turn > 0 {
			items = append(items, map[string]any{"type": "message", "role": "user", "content": "Continue"})
		}
		items = append(items,
			map[string]any{
				"type":      "function_call",
				"call_id":   "exec_command:0",
				"name":      "exec_command",
				"arguments": fmt.Sprintf(`{"cmd":"turn-%d-first"}`, turn),
			},
			map[string]any{
				"type":      "function_call",
				"call_id":   "exec_command:1",
				"name":      "exec_command",
				"arguments": fmt.Sprintf(`{"cmd":"turn-%d-second"}`, turn),
			},
			map[string]any{"type": "function_call_output", "call_id": "exec_command:0", "output": fmt.Sprintf("turn %d first", turn)},
			map[string]any{"type": "function_call_output", "call_id": "exec_command:1", "output": fmt.Sprintf("turn %d second", turn)},
		)
	}
	body, _ := json.Marshal(map[string]any{"model": strings.TrimSpace(model), "input": items})
	return body
}

func assertKimiToolMessageNamesAndIDs(t *testing.T, body []byte) {
	t.Helper()
	messages := ojson.GetBytes(body, "messages")
	if !messages.Exists() || !messages.IsArray() {
		t.Fatalf("messages missing from upstream body: %s", body)
	}
	wantIDs := make([]string, 0, 20)
	for turn := 0; turn < 10; turn++ {
		wantIDs = append(wantIDs, "exec_command:0", "exec_command:1")
	}
	var gotIDs []string
	messages.ForEach(func(_, message ojson.Result) bool {
		if message.Get("role").String() != "tool" {
			return true
		}
		if got := strings.TrimSpace(message.Get("name").String()); got != "exec_command" {
			t.Fatalf("tool name = %q, want %q; body=%s", got, "exec_command", body)
		}
		gotIDs = append(gotIDs, message.Get("tool_call_id").String())
		return true
	})
	if strings.Join(gotIDs, "\x00") != strings.Join(wantIDs, "\x00") {
		t.Fatalf("tool_call_ids = %v, want %v; body=%s", gotIDs, wantIDs, body)
	}
}
