package accountmodels

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type probeExecutor struct {
	framed, wrongNonce, incomplete bool
	calls                          int
}

func (*probeExecutor) Identifier() string { return "codex" }
func (*probeExecutor) Execute(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{}, fmt.Errorf("unused")
}
func (*probeExecutor) CountTokens(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
	return executor.Response{}, fmt.Errorf("unused")
}
func (*probeExecutor) Refresh(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return a, nil
}
func (*probeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unused")
}
func (p *probeExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, request executor.Request, _ executor.Options) (*executor.StreamResult, error) {
	p.calls++
	var body map[string]any
	_ = json.Unmarshal(request.Payload, &body)
	input := body["input"].([]any)
	var output []any
	if p.calls == 1 {
		nonce := regexp.MustCompile(`[0-9a-f]{24}`).FindString(input[0].(map[string]any)["content"].(string))
		if p.wrongNonce {
			nonce = "wrong"
		}
		args, _ := json.Marshal(map[string]string{"nonce": nonce})
		output = []any{map[string]any{"type": "function_call", "name": "proxy_sync_check", "call_id": "call_test", "arguments": string(args)}}
	} else {
		marker := input[len(input)-1].(map[string]any)["output"]
		output = []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": marker}}}}
	}
	events := []map[string]any{{"type": "response.created"}, {"type": "response.completed", "response": map[string]any{"output": output}}}
	if p.incomplete {
		events = events[:1]
	}
	chunks := make(chan executor.StreamChunk, len(events))
	for _, event := range events {
		data, _ := json.Marshal(event)
		if p.framed {
			data = append(append([]byte("data: "), data...), []byte("\n\n")...)
		}
		chunks <- executor.StreamChunk{Payload: data}
	}
	close(chunks)
	return &executor.StreamResult{Chunks: chunks}, nil
}

func TestProbeAcceptsFramedAndUnframedEventsAndRejectsIncompleteLoops(t *testing.T) {
	for _, test := range []struct {
		name                           string
		framed, wrongNonce, incomplete bool
		want                           string
	}{
		{"raw events", false, false, false, ""}, {"SSE", true, false, false, ""},
		{"wrong arguments", false, true, false, "tool call missing"}, {"missing completion", false, false, true, "probe stream incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m, a, _ := discoveryTestManager(t)
			provider := &probeExecutor{framed: test.framed, wrongNonce: test.wrongNonce, incomplete: test.incomplete}
			m.auth.RegisterExecutor(provider)
			err := m.verifyToolLoop(context.Background(), a, Model{ID: "test-model"})
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if provider.calls != 2 {
					t.Fatal("continuation was not exercised")
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v want %s", err, test.want)
			}
		})
	}
}
