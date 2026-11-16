package pluginhost

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type reasoningReplayPluginFuncs struct {
	prepare  func(context.Context, pluginapi.ReasoningReplayPrepareRequest) (pluginapi.ReasoningReplayPrepareResponse, error)
	observe  func(context.Context, pluginapi.ReasoningReplayObservation) error
	complete func(context.Context, pluginapi.ReasoningReplayCompletion) error
}

func (f reasoningReplayPluginFuncs) PrepareReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayPrepareRequest) (pluginapi.ReasoningReplayPrepareResponse, error) {
	return f.prepare(ctx, req)
}

func (f reasoningReplayPluginFuncs) ObserveReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayObservation) error {
	return f.observe(ctx, req)
}

func (f reasoningReplayPluginFuncs) CompleteReasoningReplay(ctx context.Context, req pluginapi.ReasoningReplayCompletion) error {
	return f.complete(ctx, req)
}

func TestReasoningReplayLifecycleRoutesOnlyToHandlingPlugin(t *testing.T) {
	var firstObserved, secondObserved int
	first := reasoningReplayPluginFuncs{
		prepare: func(context.Context, pluginapi.ReasoningReplayPrepareRequest) (pluginapi.ReasoningReplayPrepareResponse, error) {
			return pluginapi.ReasoningReplayPrepareResponse{}, nil
		},
		observe: func(context.Context, pluginapi.ReasoningReplayObservation) error {
			firstObserved++
			return nil
		},
		complete: func(context.Context, pluginapi.ReasoningReplayCompletion) error { return nil },
	}
	secondCompleted := false
	second := reasoningReplayPluginFuncs{
		prepare: func(_ context.Context, req pluginapi.ReasoningReplayPrepareRequest) (pluginapi.ReasoningReplayPrepareResponse, error) {
			if req.AuthID != "auth-1" || string(req.TranslatedRequest) != "translated" {
				t.Fatalf("prepare request = %#v", req)
			}
			return pluginapi.ReasoningReplayPrepareResponse{Handled: true, Body: []byte("restored")}, nil
		},
		observe: func(_ context.Context, req pluginapi.ReasoningReplayObservation) error {
			secondObserved++
			if string(req.Body) != "raw-upstream" {
				t.Fatalf("observation body = %q", req.Body)
			}
			return nil
		},
		complete: func(_ context.Context, req pluginapi.ReasoningReplayCompletion) error {
			secondCompleted = req.Outcome == pluginapi.ReasoningReplayOutcomeSucceeded
			return nil
		},
	}
	host := newHostWithRecords(
		capabilityRecord{id: "first", priority: 2, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ReasoningReplayPlugin: first}}},
		capabilityRecord{id: "second", priority: 1, plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ReasoningReplayPlugin: second}}},
	)

	resp := host.PrepareReasoningReplay(context.Background(), pluginapi.ReasoningReplayPrepareRequest{
		RequestID: "request-1", AuthID: "auth-1", TranslatedRequest: []byte("translated"),
	})
	if !resp.Handled || string(resp.Body) != "restored" {
		t.Fatalf("prepare response = %#v", resp)
	}
	host.ObserveReasoningReplay(context.Background(), pluginapi.ReasoningReplayObservation{RequestID: "request-1", Body: []byte("raw-upstream")})
	host.CompleteReasoningReplay(context.Background(), pluginapi.ReasoningReplayCompletion{RequestID: "request-1", Outcome: pluginapi.ReasoningReplayOutcomeSucceeded})
	host.ObserveReasoningReplay(context.Background(), pluginapi.ReasoningReplayObservation{RequestID: "request-1", Body: []byte("late")})

	if firstObserved != 0 || secondObserved != 1 || !secondCompleted {
		t.Fatalf("lifecycle calls = first observe %d, second observe %d, completed %v", firstObserved, secondObserved, secondCompleted)
	}
}

func TestHasReasoningReplayPluginsReflectsActiveCapability(t *testing.T) {
	if New().HasReasoningReplayPlugins() {
		t.Fatal("empty host reports an active reasoning replay plugin")
	}
	other := newHostWithRecords(capabilityRecord{id: "other", plugin: pluginapi.Plugin{}})
	if other.HasReasoningReplayPlugins() {
		t.Fatal("host without the capability reports an active reasoning replay plugin")
	}
	replay := reasoningReplayPluginFuncs{
		prepare: func(context.Context, pluginapi.ReasoningReplayPrepareRequest) (pluginapi.ReasoningReplayPrepareResponse, error) {
			return pluginapi.ReasoningReplayPrepareResponse{}, nil
		},
		observe:  func(context.Context, pluginapi.ReasoningReplayObservation) error { return nil },
		complete: func(context.Context, pluginapi.ReasoningReplayCompletion) error { return nil },
	}
	active := newHostWithRecords(capabilityRecord{id: "replay", plugin: pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ReasoningReplayPlugin: replay}}})
	if !active.HasReasoningReplayPlugins() {
		t.Fatal("host with a replay capability reports none")
	}
}

func TestReasoningReplayRPCRoundTrip(t *testing.T) {
	var observed, completed bool
	plugin := reasoningReplayPluginFuncs{
		prepare: func(_ context.Context, req pluginapi.ReasoningReplayPrepareRequest) (pluginapi.ReasoningReplayPrepareResponse, error) {
			if req.RequestID != "rpc-request" || req.AuthID != "rpc-auth" || string(req.OriginalRequest) != "original" || string(req.TranslatedRequest) != "translated" {
				t.Fatalf("prepare request = %#v", req)
			}
			return pluginapi.ReasoningReplayPrepareResponse{Handled: true, Body: []byte("restored")}, nil
		},
		observe: func(_ context.Context, req pluginapi.ReasoningReplayObservation) error {
			observed = req.RequestID == "rpc-request" && string(req.Body) == "raw"
			return nil
		},
		complete: func(_ context.Context, req pluginapi.ReasoningReplayCompletion) error {
			completed = req.RequestID == "rpc-request" && req.Outcome == pluginapi.ReasoningReplayOutcomeSucceeded
			return nil
		},
	}
	lookup := newTestSymbolLookup(nil)
	lookup.active = pluginapi.Plugin{Capabilities: pluginapi.Capabilities{ReasoningReplayPlugin: plugin}}
	adapter := &rpcPluginAdapter{id: "rpc-replay", client: lookup}

	resp, errPrepare := adapter.PrepareReasoningReplay(context.Background(), pluginapi.ReasoningReplayPrepareRequest{
		RequestID: "rpc-request", AuthID: "rpc-auth", OriginalRequest: []byte("original"), TranslatedRequest: []byte("translated"),
	})
	if errPrepare != nil || !resp.Handled || string(resp.Body) != "restored" {
		t.Fatalf("prepare response = %#v, err = %v", resp, errPrepare)
	}
	if errObserve := adapter.ObserveReasoningReplay(context.Background(), pluginapi.ReasoningReplayObservation{RequestID: "rpc-request", Body: []byte("raw")}); errObserve != nil {
		t.Fatal(errObserve)
	}
	if errComplete := adapter.CompleteReasoningReplay(context.Background(), pluginapi.ReasoningReplayCompletion{RequestID: "rpc-request", Outcome: pluginapi.ReasoningReplayOutcomeSucceeded}); errComplete != nil {
		t.Fatal(errComplete)
	}
	if !observed || !completed {
		t.Fatalf("lifecycle observed=%v completed=%v", observed, completed)
	}
}
