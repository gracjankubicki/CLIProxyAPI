package cliproxy

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/statefile"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

// localStatePersistence owns the local state files that let in-memory caches
// survive a graceful restart. Decisions are taken once at startup.
type localStatePersistence struct {
	writers []*statefile.Writer
	// Startup decisions, compared on every config reload so the operator is
	// warned when the running configuration no longer matches them.
	webSearchPath string
	affinityPath  string
	warnedFor     string
}

// newRoutingSelectorWithCache builds the routing selector and injects the
// service-owned session cache into the session affinity selector. A nil cache
// gives the selector its own private cache, as upstream does.
func newRoutingSelectorWithCache(state routingRuntimeState, sessionCache *coreauth.SessionCache) coreauth.Selector {
	return newRoutingSelectorWithCacheAndExisting(state, sessionCache, nil)
}

// newRoutingSelectorWithCacheAndExisting additionally reuses the weighted
// round-robin state of an existing selector across config reloads, as upstream
// newRoutingSelectorWithExisting does.
func newRoutingSelectorWithCacheAndExisting(state routingRuntimeState, sessionCache *coreauth.SessionCache, existing coreauth.Selector) coreauth.Selector {
	selector := fallbackSelector(state, existing)
	if !state.sessionAffinity {
		return selector
	}
	subagents := state.sessionAffinitySubagents
	return coreauth.NewSessionAffinitySelectorWithConfig(coreauth.SessionAffinityConfig{
		Fallback:         selector,
		TTL:              state.sessionAffinityTTL,
		SubagentAffinity: &subagents,
		RebindOn:         state.sessionAffinityRebindOn,
		Cache:            sessionCache,
	})
}

func fallbackSelector(state routingRuntimeState, existing coreauth.Selector) coreauth.Selector {
	switch state.strategy {
	case "weighted-round-robin":
		if existingWeighted := coreauth.UnwrapWeightedRoundRobinSelector(existing); existingWeighted != nil {
			return existingWeighted
		}
		return &coreauth.WeightedRoundRobinSelector{}
	case "fill-first":
		return &coreauth.FillFirstSelector{}
	default:
		return &coreauth.RoundRobinSelector{}
	}
}

// enableLocalStatePersistence loads state files configured at startup. Home
// mode keeps upstream behavior and never reads or writes these files.
func enableLocalStatePersistence(cfg *config.Config, sessionCache *coreauth.SessionCache) *localStatePersistence {
	persistence := &localStatePersistence{}
	webPath, affinityPath := desiredStatePaths(cfg, sessionCache != nil)
	persistence.webSearchPath, persistence.affinityPath = webPath, affinityPath
	persistence.warnedFor = webPath + "\x00" + affinityPath
	if webPath != "" {
		if writer := cache.EnableClaudeWebSearchResultPersistence(webPath, cfg.Client.Codex.WebSearchResultCache.EffectiveLimit()); writer != nil {
			persistence.writers = append(persistence.writers, writer)
		}
	}
	if affinityPath != "" {
		if writer := sessionCache.EnablePersistence(affinityPath); writer != nil {
			persistence.writers = append(persistence.writers, writer)
		}
	}
	return persistence
}

// desiredStatePaths returns the state files cfg asks for; empty means the
// feature is off, has no state file, or Home is enabled.
func desiredStatePaths(cfg *config.Config, hasSessionCache bool) (webSearch, affinity string) {
	if cfg == nil || cfg.Home.Enabled {
		return "", ""
	}
	authDir := strings.TrimSpace(cfg.AuthDir)
	if cfg.Client.Codex.WebSearchResultCache.Enabled {
		webSearch = statefile.ResolvePath(authDir, cfg.Client.Codex.WebSearchResultCache.StateFile)
	}
	if cfg.Routing.SessionAffinity && hasSessionCache {
		affinity = statefile.ResolvePath(authDir, cfg.Routing.SessionAffinityStateFile)
	}
	return webSearch, affinity
}

// warnOnReload logs a warning when a reloaded configuration asks for state
// file persistence different from what was decided at startup. Persistence is
// only switched by a restart. Each distinct mismatch is reported once.
func (p *localStatePersistence) warnOnReload(cfg *config.Config, hasSessionCache bool) {
	if p == nil || cfg == nil {
		return
	}
	webPath, affinityPath := desiredStatePaths(cfg, hasSessionCache)
	key := webPath + "\x00" + affinityPath
	if key == p.warnedFor {
		return
	}
	p.warnedFor = key
	if webPath != p.webSearchPath {
		log.Warnf("claude web search results: state file persistence changed in config (%q -> %q); it applies only after a restart, running with %q", p.webSearchPath, webPath, p.webSearchPath)
	}
	if affinityPath != p.affinityPath {
		log.Warnf("session-affinity: state file persistence changed in config (%q -> %q); it applies only after a restart, running with %q", p.affinityPath, affinityPath, p.affinityPath)
	}
}

// flush seals every writer and writes pending snapshots. It runs at the very
// end of shutdown, after the HTTP server and executors have stopped.
func (p *localStatePersistence) flush() {
	if p == nil {
		return
	}
	for _, writer := range p.writers {
		if errFlush := writer.SealAndFlush(); errFlush != nil {
			log.Warnf("%v", errFlush)
		}
	}
}
