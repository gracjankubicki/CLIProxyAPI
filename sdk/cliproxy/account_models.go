package cliproxy

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/accountmodels"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func (s *Service) discoveredAccountModels(a *coreauth.Auth) ([]*ModelInfo, bool) {
	if s.accountModels == nil {
		return nil, false
	}
	models, ok := s.accountModels.Models(a)
	if ok {
		models = withDiscoveredImageModels(a.Provider, models)
	}
	return models, ok
}

func (s *Service) accountModelAllowed(a *coreauth.Auth, id string) bool {
	cfg := s.currentConfig()
	excluded := oauthExcludedModelsForConfig(cfg, a.Provider, a.AuthKind())
	if value := strings.TrimSpace(a.Attributes["excluded_models"]); value != "" {
		excluded = strings.Split(value, ",")
	}
	return len(applyExcludedModels([]*ModelInfo{{ID: id}}, excluded)) > 0
}

func withDiscoveredImageModels(provider string, models []*ModelInfo) []*ModelInfo {
	if provider != "codex" {
		return models
	}
	// Image entitlements are not part of the Codex chat catalog. Preserve the
	// existing image endpoint definitions independently of chat discovery.
	seen := map[string]bool{}
	for _, model := range models {
		seen[model.ID] = true
	}
	for _, model := range registry.GetCodexProModels() {
		if model.Type == registry.OpenAIImageModelType && !seen[model.ID] {
			models = append(models, model)
		}
	}
	return models
}

func (s *Service) publishAccountModels(a *coreauth.Auth, models []*registry.ModelInfo) bool {
	cfg := s.currentConfig()
	if cfg == nil || !cfg.AccountModelDiscovery.Enabled || cfg.Home.Enabled || a == nil {
		return false
	}
	fence := newModelRegistrationFence(a, cfg)
	models = withDiscoveredImageModels(a.Provider, models)
	excluded := oauthExcludedModelsForConfig(cfg, a.Provider, a.AuthKind())
	if value := strings.TrimSpace(a.Attributes["excluded_models"]); value != "" {
		excluded = strings.Split(value, ",")
	}
	models = applyExcludedModels(models, excluded)
	models = applyOAuthModelAliasForAuth(cfg, a.Provider, a.AuthKind(), a.Attributes, models)
	models = s.appendPluginModels(a.Provider, models, excluded)
	models = applyOAuthSettingsForAuth(cfg, a.Provider, a.AuthKind(), models)
	models = applyModelPrefixes(models, a.Prefix, cfg.ForceModelPrefix)
	accepted := fence.publishIfCurrent(s, a.ID, func() bool {
		_, ok := registry.GetGlobalRegistry().ReplaceClientModels(a.ID, a.Provider, fence.registryEpoch, models)
		return ok
	})
	if accepted {
		s.coreManager.ReconcileRegistryModelStates(context.Background(), a.ID)
	}
	return accepted
}

func (s *Service) accountModelStatus() accountmodels.Status {
	if s.accountModels == nil {
		return accountmodels.Status{}
	}
	return s.accountModels.Status()
}

func (s *Service) refreshAccountModels() accountmodels.Status {
	if s.accountModels == nil {
		return accountmodels.Status{}
	}
	return s.accountModels.Trigger()
}
