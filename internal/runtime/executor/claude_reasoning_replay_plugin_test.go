package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

type claudeReplayPluginTestHost struct {
	prepare                pluginapi.ReasoningReplayPrepareRequest
	observe                [][]byte
	completion             []pluginapi.ReasoningReplayCompletion
	completionCtx          []context.Context
	terminate              bool
	respectCanceledContext bool
	completionSignal       chan struct{}
}

func (h *claudeReplayPluginTestHost) PrepareReasoningReplay(_ context.Context, req pluginapi.ReasoningReplayPrepareRequest) pluginapi.ReasoningReplayPrepareResponse {
	h.prepare = req
	if h.terminate {
		return pluginapi.ReasoningReplayPrepareResponse{
			Handled: true, Terminate: true, StatusCode: http.StatusBadRequest,
			ResponseBody: []byte(`{"error":{"message":"missing replay"}}`),
		}
	}
	body, _ := ojson.SetBytes(req.TranslatedRequest, "metadata.user_id", "plugin-replayed")
	return pluginapi.ReasoningReplayPrepareResponse{Handled: true, Body: body}
}

func (h *claudeReplayPluginTestHost) ObserveReasoningReplay(_ context.Context, req pluginapi.ReasoningReplayObservation) {
	h.observe = append(h.observe, bytes.Clone(req.Body))
}

func (h *claudeReplayPluginTestHost) CompleteReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayCompletion) {
	if h.respectCanceledContext && ctx != nil && ctx.Err() != nil {
		return
	}
	h.completion = append(h.completion, req)
	h.completionCtx = append(h.completionCtx, ctx)
	if h.completionSignal != nil {
		close(h.completionSignal)
	}
}

func TestClaudeExecutorReasoningReplayPluginOwnsTranslatedRequestAndRawResponse(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream body: %v", errRead)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-plugin","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`))
	}))
	defer server.Close()

	host := &claudeReplayPluginTestHost{}
	req := cliproxyexecutor.Request{Model: "claude-synthetic-plugin", Payload: []byte(`{"model":"claude-synthetic-plugin","client_metadata":{"x-codex-window-id":"window-plugin"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatClaude}
	ctx := testContextWithAPIKey("caller-plugin")
	if _, errExecute := NewClaudeExecutor(nil, host).Execute(ctx, claudeReplayTestAuth(server.URL), req, opts); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	if host.prepare.AuthID != "claude-replay-auth" || host.prepare.AuthProvider != "claude" || host.prepare.SessionID == "" {
		t.Fatalf("prepare identity = auth %q provider %q session %q", host.prepare.AuthID, host.prepare.AuthProvider, host.prepare.SessionID)
	}
	if host.prepare.Stream {
		t.Fatal("native Claude prepare Stream = true, want false")
	}
	if got := ojson.GetBytes(upstreamBody, "metadata.user_id").String(); got != "plugin-replayed" {
		t.Fatalf("upstream metadata.user_id = %q, want plugin-replayed; body=%s", got, upstreamBody)
	}
	if len(host.observe) != 1 || !bytes.Contains(host.observe[0], []byte(`"id":"msg-plugin"`)) {
		t.Fatalf("raw observations = %q", host.observe)
	}
	if len(host.completion) != 1 || host.completion[0].Outcome != pluginapi.ReasoningReplayOutcomeSucceeded {
		t.Fatalf("completion = %#v", host.completion)
	}
}

func TestCloneReasoningReplayRequestHeadersKeepsOnlySessionIdentity(t *testing.T) {
	headers := http.Header{
		"Authorization":         {"Bearer secret"},
		"Cookie":                {"session=secret"},
		"X-Api-Key":             {"secret"},
		"X-Custom-Secret":       {"secret"},
		"X-Codex-Window-Id":     {"window-1"},
		"X-Codex-Turn-Metadata": {`{"window_id":"window-1"}`},
		"Conversation_id":       {"conversation-1"},
	}

	got := cloneReasoningReplayRequestHeaders(headers)
	if got.Get("X-Codex-Window-Id") != "window-1" || got.Get("X-Codex-Turn-Metadata") == "" || got.Get("Conversation_id") != "conversation-1" {
		t.Fatalf("session identity headers = %#v", got)
	}
	for _, forbidden := range []string{"Authorization", "Cookie", "X-Api-Key", "X-Custom-Secret"} {
		if got.Get(forbidden) != "" {
			t.Fatalf("%s leaked to replay plugin headers", forbidden)
		}
	}
}

func TestClaudeExecutorReasoningReplayPluginReceivesEffectiveUpstreamStream(t *testing.T) {
	const upstreamSSE = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg-plugin-stream","model":"claude-synthetic-plugin"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":2,"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream body: %v", errRead)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	}))
	defer server.Close()

	host := &claudeReplayPluginTestHost{}
	req := cliproxyexecutor.Request{Model: "claude-synthetic-plugin", Payload: []byte(`{"model":"claude-synthetic-plugin","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
	if _, errExecute := NewClaudeExecutor(nil, host).Execute(context.Background(), claudeReplayTestAuth(server.URL), req, opts); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	if !host.prepare.Stream {
		t.Fatal("prepare Stream = false, want effective upstream stream=true")
	}
	if !ojson.GetBytes(upstreamBody, "stream").Bool() {
		t.Fatalf("upstream stream = %s, want true; body=%s", ojson.GetBytes(upstreamBody, "stream").Raw, upstreamBody)
	}
	if len(host.observe) != 1 || !bytes.Contains(host.observe[0], []byte("data: ")) || !bytes.Contains(host.observe[0], []byte("message_start")) {
		t.Fatalf("raw observations = %q, want Claude SSE", host.observe)
	}
}

func TestClaudeExecutorReasoningReplayPluginForcesExecuteUpstreamStreamAfterPayloadRule(t *testing.T) {
	const upstreamSSE = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg-plugin-execute-stream-authority","model":"claude-synthetic-plugin"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream body: %v", errRead)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	}))
	defer server.Close()

	host := &claudeReplayPluginTestHost{}
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "claude-synthetic-plugin", Protocol: "claude"}},
		Params: map[string]any{"stream": false},
	}}}}
	req := cliproxyexecutor.Request{Model: "claude-synthetic-plugin", Payload: []byte(`{"model":"claude-synthetic-plugin","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)}
	opts := cliproxyexecutor.Options{
		Stream:         false,
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
	}
	if _, errExecute := NewClaudeExecutor(cfg, host).Execute(context.Background(), claudeReplayTestAuth(server.URL), req, opts); errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}

	if !host.prepare.Stream {
		t.Fatal("plugin prepare Stream = false, want true")
	}
	if got := ojson.GetBytes(host.prepare.TranslatedRequest, "stream"); !got.Exists() || !got.Bool() {
		t.Fatalf("plugin translated stream = %s, want true; body=%s", got.Raw, host.prepare.TranslatedRequest)
	}
	if got := ojson.GetBytes(upstreamBody, "stream"); !got.Exists() || !got.Bool() {
		t.Fatalf("upstream stream = %s, want true; body=%s", got.Raw, upstreamBody)
	}
}

func TestClaudeExecutorReasoningReplayPluginCanRejectBeforeUpstream(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()

	host := &claudeReplayPluginTestHost{terminate: true}
	req := cliproxyexecutor.Request{Model: "claude-synthetic-plugin", Payload: []byte(`{"model":"claude-synthetic-plugin","input":[]}`)}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse, ResponseFormat: sdktranslator.FormatClaude}
	_, errExecute := NewClaudeExecutor(nil, host).Execute(context.Background(), claudeReplayTestAuth(server.URL), req, opts)
	var terminated *cliproxyexecutor.RequestTerminatedError
	if !errors.As(errExecute, &terminated) || terminated.StatusCode() != http.StatusBadRequest {
		t.Fatalf("Execute() error = %v, want direct 400", errExecute)
	}
	if called {
		t.Fatal("upstream was called after plugin rejection")
	}
	if len(host.completion) != 1 || host.completion[0].StatusCode != http.StatusBadRequest {
		t.Fatalf("completion = %#v", host.completion)
	}
}

func TestClaudeReasoningReplayAttemptCompletesWithDetachedContextAfterCancellation(t *testing.T) {
	type contextKey string
	const key contextKey = "replay-test"

	base := context.WithValue(context.Background(), key, "value")
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	cancel()

	host := &claudeReplayPluginTestHost{respectCanceledContext: true}
	attempt := &claudeReasoningReplayAttempt{host: host, requestID: "canceled-request"}
	attempt.complete(ctx, context.Canceled)

	if len(host.completion) != 1 {
		t.Fatalf("completion count = %d, want 1", len(host.completion))
	}
	if got := host.completion[0].Outcome; got != pluginapi.ReasoningReplayOutcomeCanceled {
		t.Fatalf("completion outcome = %q, want canceled", got)
	}
	if len(host.completionCtx) != 1 || host.completionCtx[0] == nil {
		t.Fatal("completion context was not delivered")
	}
	completionCtx := host.completionCtx[0]
	if completionCtx.Err() != nil {
		t.Fatalf("completion context error = %v, want nil", completionCtx.Err())
	}
	if completionCtx.Done() != nil {
		t.Fatal("completion context is still cancelable")
	}
	if got := completionCtx.Value(key); got != "value" {
		t.Fatalf("completion context value = %v, want value", got)
	}
	if _, ok := completionCtx.Deadline(); ok {
		t.Fatal("completion context unexpectedly retained a deadline")
	}
}

func TestClaudeReasoningReplayAttemptPreservesSuccessAfterLateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	host := &claudeReplayPluginTestHost{}
	attempt := &claudeReasoningReplayAttempt{host: host, requestID: "completed-before-cancel"}
	attempt.complete(ctx, nil)

	if len(host.completion) != 1 {
		t.Fatalf("completion count = %d, want 1", len(host.completion))
	}
	if got := host.completion[0].Outcome; got != pluginapi.ReasoningReplayOutcomeSucceeded {
		t.Fatalf("completion outcome = %q, want succeeded", got)
	}
}

func TestClaudeExecutorExecuteStreamReasoningReplayPluginForcesUpstreamStream(t *testing.T) {
	const upstreamSSE = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg-plugin-stream-authority","model":"claude-synthetic-plugin"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var errRead error
		upstreamBody, errRead = io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read upstream body: %v", errRead)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(upstreamSSE))
	}))
	defer server.Close()

	host := &claudeReplayPluginTestHost{}
	cfg := &config.Config{Payload: config.PayloadConfig{Override: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "claude-synthetic-plugin", Protocol: "claude"}},
		Params: map[string]any{"stream": false},
	}}}}
	req := cliproxyexecutor.Request{Model: "claude-synthetic-plugin", Payload: []byte(`{"model":"claude-synthetic-plugin","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatClaude,
	}
	result, errExecute := NewClaudeExecutor(cfg, host).ExecuteStream(context.Background(), claudeReplayTestAuth(server.URL), req, opts)
	if errExecute != nil {
		t.Fatalf("ExecuteStream() error = %v", errExecute)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	if !host.prepare.Stream {
		t.Fatal("plugin prepare Stream = false, want true")
	}
	if got := ojson.GetBytes(host.prepare.TranslatedRequest, "stream"); !got.Exists() || !got.Bool() {
		t.Fatalf("plugin translated stream = %s, want true; body=%s", got.Raw, host.prepare.TranslatedRequest)
	}
	if got := ojson.GetBytes(upstreamBody, "stream"); !got.Exists() || !got.Bool() {
		t.Fatalf("upstream stream = %s, want true; body=%s", got.Raw, upstreamBody)
	}
	if len(host.observe) == 0 {
		t.Fatal("plugin received no raw stream observations")
	}
	if len(host.completion) != 1 || host.completion[0].Outcome != pluginapi.ReasoningReplayOutcomeSucceeded {
		t.Fatalf("completion = %#v, want one succeeded completion", host.completion)
	}
}

func TestWrapClaudeReasoningReplayCompletionCoversStreamLifecycle(t *testing.T) {
	streamErr := errors.New("upstream stream failed")
	tests := []struct {
		name      string
		want      pluginapi.ReasoningReplayOutcome
		streamErr error
		cancel    bool
	}{
		{name: "success", want: pluginapi.ReasoningReplayOutcomeSucceeded},
		{name: "cancel", want: pluginapi.ReasoningReplayOutcomeCanceled, cancel: true},
		{name: "error", want: pluginapi.ReasoningReplayOutcomeFailed, streamErr: streamErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			host := &claudeReplayPluginTestHost{completionSignal: make(chan struct{})}
			attempt := &claudeReasoningReplayAttempt{host: host, requestID: "stream-" + tt.name}
			input := make(chan cliproxyexecutor.StreamChunk)
			result := wrapClaudeReasoningReplayCompletion(ctx, &cliproxyexecutor.StreamResult{
				Headers: http.Header{"X-Test": []string{"stream"}},
				Chunks:  input,
			}, attempt)
			continueSending := make(chan struct{})
			go func() {
				attempt.observe(ctx, []byte("raw-first"))
				input <- cliproxyexecutor.StreamChunk{Payload: []byte("first")}
				if tt.cancel {
					<-continueSending
					input <- cliproxyexecutor.StreamChunk{Payload: []byte("second")}
					close(input)
					return
				}
				input <- cliproxyexecutor.StreamChunk{Payload: []byte("second")}
				if tt.streamErr != nil {
					input <- cliproxyexecutor.StreamChunk{Err: tt.streamErr}
				}
				close(input)
			}()

			first, ok := <-result.Chunks
			if !ok || string(first.Payload) != "first" {
				t.Fatalf("first chunk = %#v, ok=%v", first, ok)
			}
			if tt.cancel {
				cancel()
				close(continueSending)
				<-host.completionSignal
			}
			for range result.Chunks {
			}

			if len(host.observe) != 1 || string(host.observe[0]) != "raw-first" {
				t.Fatalf("observations = %q, want raw-first", host.observe)
			}
			if len(host.completion) != 1 {
				t.Fatalf("completion count = %d, want 1", len(host.completion))
			}
			if got := host.completion[0].Outcome; got != tt.want {
				t.Fatalf("completion outcome = %q, want %q", got, tt.want)
			}
			if tt.streamErr != nil && host.completion[0].Error != "upstream_error" {
				t.Fatalf("completion error = %q, want bounded upstream_error category", host.completion[0].Error)
			}
		})
	}
}

func TestClaudeReasoningReplayCompletionDoesNotForwardErrorDetails(t *testing.T) {
	const secretMarker = "replay-secret-marker-must-not-cross-plugin-boundary"
	tests := []struct {
		name       string
		err        error
		wantError  string
		wantStatus int
	}{
		{
			name: "status error with upstream response body",
			err: &cliproxyexecutor.RequestTerminatedError{
				HTTPStatus: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(`{"error":"` + secretMarker + `"}`),
			},
			wantError:  "http_error",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:      "ordinary error",
			err:       errors.New("upstream failed with " + secretMarker),
			wantError: "upstream_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := &claudeReplayPluginTestHost{}
			attempt := &claudeReasoningReplayAttempt{host: host, requestID: "redacted-" + strings.ReplaceAll(tt.name, " ", "-")}
			attempt.complete(context.Background(), tt.err)

			if len(host.completion) != 1 {
				t.Fatalf("completion count = %d, want 1", len(host.completion))
			}
			completion := host.completion[0]
			if completion.Error != tt.wantError || completion.StatusCode != tt.wantStatus {
				t.Fatalf("completion error/status = %q/%d, want %q/%d", completion.Error, completion.StatusCode, tt.wantError, tt.wantStatus)
			}
			if strings.Contains(completion.Error, secretMarker) {
				t.Fatalf("completion error leaked secret marker: %q", completion.Error)
			}
		})
	}
}

func TestWrapClaudeReasoningReplayCompletionPreservesCompletedUpstreamAfterLateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := &claudeReplayPluginTestHost{completionSignal: make(chan struct{})}
	attempt := &claudeReasoningReplayAttempt{host: host, requestID: "stream-complete-before-cancel"}
	input := make(chan cliproxyexecutor.StreamChunk)
	result := wrapClaudeReasoningReplayCompletion(ctx, &cliproxyexecutor.StreamResult{Chunks: input}, attempt)

	go func() {
		input <- cliproxyexecutor.StreamChunk{Payload: []byte("tool-call")}
	}()
	first, ok := <-result.Chunks
	if !ok || string(first.Payload) != "tool-call" {
		t.Fatalf("first chunk = %#v, ok=%v", first, ok)
	}

	attempt.observe(ctx, []byte("data: {\"type\":\"message_stop\"}\n"))
	cancel()
	go func() {
		input <- cliproxyexecutor.StreamChunk{Payload: []byte("response-completed")}
	}()
	<-host.completionSignal
	close(input)
	for range result.Chunks {
	}

	if len(host.completion) != 1 {
		t.Fatalf("completion count = %d, want 1", len(host.completion))
	}
	if got := host.completion[0].Outcome; got != pluginapi.ReasoningReplayOutcomeSucceeded {
		t.Fatalf("completion outcome = %q, want succeeded", got)
	}
}

type claudeReplayAvailabilityTestHost struct {
	claudeReplayPluginTestHost
	available bool
	prepared  int
}

func (h *claudeReplayAvailabilityTestHost) HasReasoningReplayPlugins() bool { return h.available }

func (h *claudeReplayAvailabilityTestHost) PrepareReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayPrepareRequest) pluginapi.ReasoningReplayPrepareResponse {
	h.prepared++
	return pluginapi.ReasoningReplayPrepareResponse{}
}

func TestPrepareReasoningReplayPluginDocKeepsDocWithoutActivePlugin(t *testing.T) {
	for _, tc := range []struct {
		name         string
		available    bool
		wantPrepared int
	}{
		{name: "no active plugin", available: false, wantPrepared: 0},
		{name: "active plugin does not handle", available: true, wantPrepared: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &claudeReplayAvailabilityTestHost{available: tc.available}
			e := NewClaudeExecutor(nil, host)
			doc := ojson.NewDocString(`{"model":"claude-test","messages":[]}`)
			got, attempt, err := e.prepareReasoningReplayPluginDoc(context.Background(), nil, cliproxyexecutor.Request{Model: "claude-test"}, cliproxyexecutor.Options{}, "claude", doc, true)
			if err != nil || attempt != nil {
				t.Fatalf("prepare = attempt %v, err %v; want nil, nil", attempt, err)
			}
			if got != doc {
				t.Fatal("replay preparation replaced the request Doc although no plugin handled it")
			}
			if host.prepared != tc.wantPrepared {
				t.Fatalf("PrepareReasoningReplay calls = %d, want %d", host.prepared, tc.wantPrepared)
			}
		})
	}
}

// Config reloads reuse the executor through WithConfig; the replay host bound
// at construction must survive, otherwise replay silently turns off.
func TestClaudeExecutorWithConfigKeepsReasoningReplayHost(t *testing.T) {
	host := &claudeReplayAvailabilityTestHost{available: true}
	original := NewClaudeExecutor(&config.Config{}, host)
	reloadedCfg := &config.Config{}
	reloaded := original.WithConfig(reloadedCfg)
	if reloaded.cfg != reloadedCfg {
		t.Fatal("WithConfig did not apply the new config")
	}
	if reloaded.reasoningReplay != host {
		t.Fatalf("WithConfig reasoningReplay = %v, want the construction host", reloaded.reasoningReplay)
	}
	if reloaded.threadBetas != original.threadBetas {
		t.Fatal("WithConfig dropped the shared thread beta store")
	}
}
