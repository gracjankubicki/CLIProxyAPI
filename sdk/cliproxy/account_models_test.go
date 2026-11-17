package cliproxy

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAccountModelPublicationPreservesCooldownAndHonorsExclusions(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	a, err := manager.Register(context.Background(), &coreauth.Auth{ID: "account-discovery-quota-test", Provider: "claude", Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	r := registry.GetGlobalRegistry()
	t.Cleanup(func() { r.UnregisterClient(a.ID) })
	r.RegisterClient(a.ID, "claude", []*ModelInfo{{ID: "discovery-keep", Type: "claude"}})
	retry := time.Hour
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: a.ID, Provider: "claude", Model: "discovery-keep", Success: false, RetryAfter: &retry, Error: &coreauth.Error{HTTPStatus: 429, Message: "quota"}})
	before, _ := manager.GetByID(a.ID)
	if before.ModelStates["discovery-keep"] == nil || !before.ModelStates["discovery-keep"].Unavailable {
		t.Fatal("test did not establish a cooldown")
	}
	cfg := &config.Config{AccountModelDiscovery: config.AccountModelDiscoveryConfig{Enabled: true}, OAuthExcludedModels: map[string][]string{"claude": {"discovery-blocked"}}}
	s := &Service{cfg: cfg, coreManager: manager}
	if !s.publishAccountModels(before, []*ModelInfo{{ID: "discovery-keep", Type: "claude", AccountDiscovered: true, ContextLength: 1000000}, {ID: "discovery-added", Type: "claude"}, {ID: "discovery-blocked", Type: "claude"}}) {
		t.Fatal("publication rejected")
	}
	after, _ := manager.GetByID(a.ID)
	if !reflect.DeepEqual(before.ModelStates["discovery-keep"], after.ModelStates["discovery-keep"]) || !reflect.DeepEqual(before.Quota, after.Quota) {
		t.Fatal("discovery reset cooldown or quota")
	}
	if !r.ClientSupportsModel(a.ID, "discovery-added") || r.ClientSupportsModel(a.ID, "discovery-blocked") {
		t.Fatal("exclusions or per-account models lost")
	}
}

func TestAccountModelPublicationRejectsDisabledSnapshot(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	a, err := manager.Register(context.Background(), &coreauth.Auth{ID: "account-discovery-stale-test", Provider: "claude", Status: coreauth.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	r := registry.GetGlobalRegistry()
	t.Cleanup(func() { r.UnregisterClient(a.ID) })
	s := &Service{cfg: &config.Config{AccountModelDiscovery: config.AccountModelDiscoveryConfig{Enabled: true}}, coreManager: manager}
	updated := a.Clone()
	updated.Disabled = true
	_, err = manager.Update(context.Background(), updated)
	if err != nil {
		t.Fatal(err)
	}
	if s.publishAccountModels(a, []*ModelInfo{{ID: "discovery-stale-model"}}) || r.ClientSupportsModel(a.ID, "discovery-stale-model") {
		t.Fatal("disabled credential was republished")
	}
}
