package executor

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func claudeResponsesReplaySessionKey(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) string {
	if value := codexReasoningReplaySessionKeyFromPayload(req.Payload); value != "" {
		return value
	}
	if value := codexReasoningReplaySessionKeyFromHeaders(opts.Headers); value != "" {
		return value
	}
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		return codexReasoningReplaySessionKeyFromHeaders(ginCtx.Request.Header)
	}
	return ""
}

type claudeReasoningReplayAttempt struct {
	host              pluginapi.ReasoningReplayHost
	requestID         string
	once              sync.Once
	upstreamCompleted atomic.Bool
}

func detachedClaudeReasoningReplayContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return context.WithoutCancel(ctx)
}

// reasoningReplayAvailability is implemented by hosts that can tell whether a
// replay plugin is active right now.
type reasoningReplayAvailability interface {
	HasReasoningReplayPlugins() bool
}

// prepareReasoningReplayPluginDoc runs replay preparation on the request Doc.
// The body is materialized only when a replay plugin is active, so the default
// path keeps the same Doc and the single-Doc edit pipeline.
func (e *ClaudeExecutor) prepareReasoningReplayPluginDoc(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, toFormat string, doc *ojson.Doc, stream bool) (*ojson.Doc, *claudeReasoningReplayAttempt, error) {
	if e == nil || e.reasoningReplay == nil {
		return doc, nil, nil
	}
	if availability, ok := e.reasoningReplay.(reasoningReplayAvailability); ok && !availability.HasReasoningReplayPlugins() {
		return doc, nil, nil
	}
	body, attempt, err := e.prepareReasoningReplayPlugin(ctx, auth, req, opts, toFormat, doc.Bytes(), stream)
	if attempt == nil && err == nil {
		// Not handled: the plugin left the request unchanged.
		return doc, nil, nil
	}
	return ojson.NewDoc(body), attempt, err
}

func (e *ClaudeExecutor) prepareReasoningReplayPlugin(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, toFormat string, translated []byte, stream bool) ([]byte, *claudeReasoningReplayAttempt, error) {
	if e == nil || e.reasoningReplay == nil {
		return translated, nil, nil
	}
	requestID := uuid.NewString()
	sessionID := claudeResponsesReplaySessionKey(ctx, req, opts)
	sessionID = xaiReasoningReplayIsolateSessionKey(ctx, sessionID)
	resp := e.reasoningReplay.PrepareReasoningReplay(ctx, pluginapi.ReasoningReplayPrepareRequest{
		RequestID:         requestID,
		AuthID:            replayAuthID(auth),
		AuthProvider:      replayAuthProvider(auth),
		AuthKind:          replayAuthKind(auth),
		SessionID:         sessionID,
		SourceFormat:      opts.SourceFormat.String(),
		ToFormat:          toFormat,
		Model:             req.Model,
		Stream:            stream,
		Headers:           cloneReasoningReplayRequestHeaders(opts.Headers),
		OriginalRequest:   bytes.Clone(req.Payload),
		TranslatedRequest: bytes.Clone(translated),
	})
	if !resp.Handled {
		return translated, nil, nil
	}
	attempt := &claudeReasoningReplayAttempt{host: e.reasoningReplay, requestID: requestID}
	if resp.Terminate {
		statusCode := resp.StatusCode
		if statusCode < http.StatusBadRequest || statusCode > 599 {
			statusCode = http.StatusBadRequest
		}
		return translated, attempt, &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: statusCode,
			Header:     cloneReplayHeaders(resp.ResponseHeaders),
			Body:       bytes.Clone(resp.ResponseBody),
		}
	}
	if len(resp.Body) == 0 {
		return translated, attempt, nil
	}
	return bytes.Clone(resp.Body), attempt, nil
}

func (a *claudeReasoningReplayAttempt) observe(ctx context.Context, body []byte) {
	if a == nil || a.host == nil || strings.TrimSpace(a.requestID) == "" || len(body) == 0 {
		return
	}
	var messageID string
	completed := false
	observeClaudeSSEEvent(body, &messageID, &completed, nil)
	if completed {
		a.upstreamCompleted.Store(true)
	}
	a.host.ObserveReasoningReplay(ctx, pluginapi.ReasoningReplayObservation{RequestID: a.requestID, Body: bytes.Clone(body)})
}

func (a *claudeReasoningReplayAttempt) complete(ctx context.Context, err error) {
	if a == nil || a.host == nil || strings.TrimSpace(a.requestID) == "" {
		return
	}
	a.once.Do(func() {
		completion := pluginapi.ReasoningReplayCompletion{RequestID: a.requestID, Outcome: pluginapi.ReasoningReplayOutcomeSucceeded}
		// A nil terminal error means the upstream response completed. The request
		// context may be canceled later when the client starts its tool calls.
		if err != nil {
			completion.Outcome = pluginapi.ReasoningReplayOutcomeFailed
			completion.Error, completion.StatusCode = classifyClaudeReasoningReplayError(ctx, err)
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (ctx != nil && ctx.Err() != nil) {
				completion.Outcome = pluginapi.ReasoningReplayOutcomeCanceled
			}
		}
		a.host.CompleteReasoningReplay(detachedClaudeReasoningReplayContext(ctx), completion)
	})
}

// classifyClaudeReasoningReplayError deliberately returns only a bounded
// category and a validated HTTP status. Error() may contain provider response
// bodies, credentials, or other request data and must never cross the plugin
// boundary in a completion event.
func classifyClaudeReasoningReplayError(ctx context.Context, err error) (string, int) {
	if err == nil {
		return "", 0
	}

	statusCode := 0
	var statusErr interface{ StatusCode() int }
	if errors.As(err, &statusErr) {
		candidate := statusErr.StatusCode()
		if candidate >= http.StatusBadRequest && candidate <= 599 {
			statusCode = candidate
		}
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)):
		return "deadline_exceeded", statusCode
	case errors.Is(err, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)):
		return "canceled", statusCode
	case statusCode != 0:
		return "http_error", statusCode
	default:
		return "upstream_error", 0
	}
}

func replayAuthID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.ID)
}

func replayAuthProvider(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.Provider)
}

func replayAuthKind(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	return strings.TrimSpace(auth.AuthKind())
}

func cloneReplayHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	return headers.Clone()
}

func cloneReasoningReplayRequestHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	allowed := map[string]struct{}{
		"x-codex-turn-metadata": {},
		"x-codex-window-id":     {},
		"session_id":            {},
		"session-id":            {},
		"conversation_id":       {},
	}
	cloned := make(http.Header)
	for name, values := range headers {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(name))]; !ok {
			continue
		}
		cloned[name] = append([]string(nil), values...)
	}
	if len(cloned) == 0 {
		return nil
	}
	return cloned
}

func wrapClaudeReasoningReplayCompletion(ctx context.Context, result *cliproxyexecutor.StreamResult, attempt *claudeReasoningReplayAttempt) *cliproxyexecutor.StreamResult {
	if result == nil || attempt == nil {
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var terminalErr error
		for chunk := range result.Chunks {
			if chunk.Err != nil && terminalErr == nil {
				terminalErr = chunk.Err
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				if terminalErr == nil && !attempt.upstreamCompleted.Load() {
					terminalErr = ctx.Err()
				}
				attempt.complete(ctx, terminalErr)
				return
			}
		}
		attempt.complete(ctx, terminalErr)
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers.Clone(), Chunks: out}
}
