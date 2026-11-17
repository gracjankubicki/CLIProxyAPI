package accountmodels

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func discoveryTestManager(t *testing.T) (*Manager, *coreauth.Auth, map[string][]*registry.ModelInfo) {
	t.Helper()
	if err := kvstore.ConfigureLocal(true, filepath.Join(t.TempDir(), "cache.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kvstore.ShutdownLocal() })
	auth := coreauth.NewManager(nil, nil, nil)
	a, err := auth.Register(context.Background(), &coreauth.Auth{ID: "discovery-account", Provider: "codex", Status: coreauth.StatusActive, Metadata: map[string]any{"account_id": "test-account"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AccountModelDiscovery: config.AccountModelDiscoveryConfig{Enabled: true}}
	published := map[string][]*registry.ModelInfo{}
	m := New(auth, func() *config.Config { return cfg }, func(a *coreauth.Auth, models []*registry.ModelInfo) bool { published[a.ID] = models; return true }, nil)
	m.fetch = func(context.Context, *coreauth.Auth) ([]byte, error) { return []byte(codexFixture), nil }
	m.probe = func(context.Context, *coreauth.Auth, Model) error { return nil }
	return m, a, published
}

func TestDiscoveryPersistsAndUsesOnlyAccountModels(t *testing.T) {
	m, a, published := discoveryTestManager(t)
	probes := 0
	m.probe = func(context.Context, *coreauth.Auth, Model) error { probes++; return nil }
	m.refresh(context.Background())
	if len(published[a.ID]) != 1 || published[a.ID][0].ID != "gpt-discovery-probe" || probes != 1 {
		t.Fatal(published, probes)
	}
	m.refresh(context.Background())
	if probes != 1 {
		t.Fatal("unchanged model retested")
	}
	m.snapshots = map[string]Snapshot{}
	models, ok := m.Models(a)
	if !ok || len(models) != 1 || models[0].ContextLength != 500000 || len(models[0].CodexClientMetadata) == 0 {
		t.Fatal("cache lost metadata")
	}
	status := m.Status()
	if len(status.Accounts) != 1 || len(status.Accounts[0].Models[0].ClientMetadata) != 0 {
		t.Fatal("status leaks full client prompt metadata")
	}
}

func TestFailedDiscoveryRetainsLastGoodButEmptyCatalogRemoves(t *testing.T) {
	m, a, published := discoveryTestManager(t)
	m.refresh(context.Background())
	m.fetch = func(context.Context, *coreauth.Auth) ([]byte, error) {
		return nil, fmt.Errorf("TOKEN_SECRET_SHOULD_NOT_LEAK")
	}
	m.refresh(context.Background())
	if len(published[a.ID]) != 1 {
		t.Fatal("network failure removed models")
	}
	s := m.Status()
	if len(s.Accounts) != 1 || s.Accounts[0].Error == "" || strings.Contains(s.Accounts[0].Error, "SECRET") {
		t.Fatal("unsafe error", s)
	}
	m.fetch = func(context.Context, *coreauth.Auth) ([]byte, error) { return []byte(`{"models":[]}`), nil }
	m.refresh(context.Background())
	if len(published[a.ID]) != 0 {
		t.Fatal("successful empty catalog was not applied")
	}
	if models, ok := m.Models(a); !ok || len(models) != 0 {
		t.Fatal("empty last-good snapshot not cached")
	}
}

func TestChangedParametersNeedProbeAndKeepLastGoodOnFailure(t *testing.T) {
	m, a, published := discoveryTestManager(t)
	m.refresh(context.Background())
	m.fetch = func(context.Context, *coreauth.Auth) ([]byte, error) {
		return []byte(strings.Replace(codexFixture, "500000", "600000", 1)), nil
	}
	m.probe = func(context.Context, *coreauth.Auth, Model) error { return fmt.Errorf("probe HTTP 429") }
	m.refresh(context.Background())
	if published[a.ID][0].ContextLength != 500000 {
		t.Fatal("failed check replaced last-good parameters")
	}
	s := m.Status().Accounts[0].Models[0]
	if s.PendingParameters == nil || s.PendingParameters.ContextWindow != 600000 || s.PendingVerification == nil || s.PendingVerification.Status != "failed" {
		t.Fatal("pending update not reported")
	}
}

func TestNewFailedModelNotPublishedAndExcludedModelNotProbed(t *testing.T) {
	m, a, published := discoveryTestManager(t)
	m.probe = func(context.Context, *coreauth.Auth, Model) error { return fmt.Errorf("tool call missing") }
	m.refresh(context.Background())
	if len(published[a.ID]) != 0 {
		t.Fatal("failed new model published")
	}
	probes := 0
	m.allowed = func(*coreauth.Auth, string) bool { return false }
	m.probe = func(context.Context, *coreauth.Auth, Model) error { probes++; return nil }
	m.refresh(context.Background())
	if probes != 0 {
		t.Fatal("excluded model used inference")
	}
}

func TestAccountsRemainDistinctAndStaleAccountNotPublished(t *testing.T) {
	m, a, published := discoveryTestManager(t)
	b, err := m.auth.Register(context.Background(), &coreauth.Auth{ID: "second", Provider: "codex", Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	m.fetch = func(_ context.Context, auth *coreauth.Auth) ([]byte, error) {
		return []byte(strings.Replace(codexFixture, "gpt-discovery-probe", "model-"+auth.ID, 1)), nil
	}
	m.refresh(context.Background())
	if len(published[a.ID]) != 1 || len(published[b.ID]) != 1 || published[a.ID][0].ID == published[b.ID][0].ID {
		t.Fatal("cross-account model leakage")
	}
	delete(published, a.ID)
	m.fetch = func(_ context.Context, current *coreauth.Auth) ([]byte, error) {
		if current.ID == a.ID {
			current.Disabled = true
			_, _ = m.auth.Update(context.Background(), current)
		}
		return []byte(codexFixture), nil
	}
	m.refresh(context.Background())
	if _, ok := published[a.ID]; ok {
		t.Fatal("disabled account received a stale publication")
	}
}

func TestCoolingAccountDefersNewParametersWithoutInference(t *testing.T) {
	m, a, published := discoveryTestManager(t)
	m.refresh(context.Background())
	a.Unavailable = true
	a.NextRetryAfter = time.Now().Add(time.Hour)
	if _, err := m.auth.Update(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	m.fetch = func(context.Context, *coreauth.Auth) ([]byte, error) {
		return []byte(strings.Replace(codexFixture, "500000", "600000", 1)), nil
	}
	m.probe = func(context.Context, *coreauth.Auth, Model) error {
		t.Fatal("cooling account used inference")
		return nil
	}
	m.refresh(context.Background())
	if published[a.ID][0].ContextLength != 500000 {
		t.Fatal("cooldown replaced last-good limits")
	}
	model := m.Status().Accounts[0].Models[0]
	if model.PendingVerification == nil || model.PendingVerification.Status != "deferred" {
		t.Fatal("missing deferred update")
	}
}

func TestAccountAddedDuringRefreshQueuesOneFollowUp(t *testing.T) {
	m, _, _ := discoveryTestManager(t)
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	m.fetch = func(context.Context, *coreauth.Auth) ([]byte, error) {
		if first {
			first = false
			close(entered)
			<-release
		}
		return []byte(codexFixture), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	b, err := m.auth.Register(ctx, &coreauth.Auth{ID: "arriving-account", Provider: "codex", Status: coreauth.StatusActive})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		m.Models(b)
		m.Trigger()
	}
	if len(m.wake) != 1 {
		close(release)
		t.Fatal("missing or duplicated follow-up")
	}
	close(release)
	deadline := time.After(3 * time.Second)
	for {
		status := m.Status()
		if !status.Running && len(status.Accounts) == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("new account missed refresh")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestProbeRespectsModelCooldownAndAllowsExpiredRetry(t *testing.T) {
	now := time.Now()
	a := &coreauth.Auth{Unavailable: true, NextRetryAfter: now.Add(-time.Second)}
	if probeBlocked(a, "new-model", now) {
		t.Fatal("expired retry stayed blocked")
	}
	a.ModelStates = map[string]*coreauth.ModelState{"new-model": {Unavailable: true, NextRetryAfter: now.Add(time.Minute)}}
	if !probeBlocked(a, "new-model", now) {
		t.Fatal("per-model cooldown bypassed")
	}
	if probeBlocked(a, "other-model", now) {
		t.Fatal("model cooldown leaked")
	}
}
