package cliproxy

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func persistenceConfig(authDir string) *internalconfig.Config {
	cfg := &internalconfig.Config{}
	cfg.AuthDir = authDir
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionAffinityStateFile = ".cliproxy-state/session-affinity.state"
	cfg.Client.Codex.WebSearchResultCache.Enabled = true
	cfg.Client.Codex.WebSearchResultCache.StateFile = ".cliproxy-state/claude-web-search-results.state"
	return cfg
}

func writeSentinel(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSentinel(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "sentinel" {
		t.Fatalf("%s touched: %q, %v", path, got, err)
	}
	if matches, _ := filepath.Glob(path + ".corrupt-*"); len(matches) != 0 {
		t.Fatalf("%s quarantined: %v", path, matches)
	}
}

func TestLocalStatePersistenceIgnoresFilesInHomeModeAndWhenDisabled(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*internalconfig.Config)
	}{
		{"home", func(cfg *internalconfig.Config) { cfg.Home.Enabled = true }},
		{"disabled", func(cfg *internalconfig.Config) {
			cfg.Routing.SessionAffinity = false
			cfg.Client.Codex.WebSearchResultCache.Enabled = false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := persistenceConfig(dir)
			tc.mutate(cfg)
			affinityPath := filepath.Join(dir, cfg.Routing.SessionAffinityStateFile)
			webPath := filepath.Join(dir, cfg.Client.Codex.WebSearchResultCache.StateFile)
			writeSentinel(t, affinityPath)
			writeSentinel(t, webPath)

			sessionCache := coreauth.NewSessionCache(time.Hour)
			defer sessionCache.Stop()
			persistence := enableLocalStatePersistence(cfg, sessionCache)
			if len(persistence.writers) != 0 {
				t.Fatalf("writers = %d, want none", len(persistence.writers))
			}
			sessionCache.Set("s", "auth-a")
			cache.CacheClaudeWebSearchResult("s", "t", []byte(`[1]`), time.Hour, 8)
			persistence.flush()
			assertSentinel(t, affinityPath)
			assertSentinel(t, webPath)
		})
	}
}

func TestLocalStatePersistenceSurvivesRestartAndHotReload(t *testing.T) {
	dir := t.TempDir()
	cfg := persistenceConfig(dir)
	cfg.Client.Codex.WebSearchResultCache.Enabled = false // web search covered in internal/cache
	auths := []*coreauth.Auth{{ID: "auth-a"}, {ID: "auth-b"}}
	headers := http.Header{}
	headers.Set("Session-Id", "019a0000-1111-2222-3333-444455556666")
	opts := cliproxyexecutor.Options{Headers: headers, OriginalRequest: []byte(`{"input":"hi"}`)}
	pickFrom := func(selector coreauth.Selector, candidates []*coreauth.Auth) string {
		got, err := selector.Pick(context.Background(), "mixed", "gpt-6.1-sol", opts, candidates)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		return got.ID
	}
	pick := func(selector coreauth.Selector) string { return pickFrom(selector, auths) }

	state := normalizedRoutingRuntimeState(cfg)
	sessionCache := coreauth.NewSessionCache(state.sessionAffinityTTL)
	persistence := enableLocalStatePersistence(cfg, sessionCache)
	if len(persistence.writers) != 1 {
		t.Fatalf("writers = %d, want 1", len(persistence.writers))
	}
	selector := newRoutingSelectorWithCache(state, sessionCache)
	// Bind to auth-b, which fill-first would never choose on a cold pick.
	bound := pickFrom(selector, auths[1:])
	if bound != "auth-b" {
		t.Fatalf("initial binding = %s", bound)
	}

	// Hot reload: TTL and strategy change rebuild the selector; the old one is stopped.
	cfg.Routing.SessionAffinityTTL = "2h"
	cfg.Routing.Strategy = "fill-first"
	reloaded := newRoutingSelectorWithCache(normalizedRoutingRuntimeState(cfg), sessionCache)
	selector.(coreauth.StoppableSelector).Stop()
	if got := pick(reloaded); got != bound {
		t.Fatalf("hot reload lost binding: %s -> %s", bound, got)
	}

	// Graceful shutdown then a fresh process.
	persistence.flush()
	reloaded.(coreauth.StoppableSelector).Stop()
	sessionCache.Stop()

	restartedCache := coreauth.NewSessionCache(time.Hour)
	defer restartedCache.Stop()
	restarted := enableLocalStatePersistence(cfg, restartedCache)
	defer restarted.flush()
	fresh := newRoutingSelectorWithCache(normalizedRoutingRuntimeState(cfg), restartedCache)
	defer fresh.(coreauth.StoppableSelector).Stop()
	if got := pick(fresh); got != bound {
		t.Fatalf("after restart picked %s, want bound %s", got, bound)
	}
	// Control: a session without a stored binding takes the cold fill-first choice.
	other := cliproxyexecutor.Options{Headers: http.Header{"Session-Id": []string{"019a9999-0000-0000-0000-000000000000"}}, OriginalRequest: []byte(`{"input":"x"}`)}
	if got, _ := fresh.Pick(context.Background(), "mixed", "gpt-6.1-sol", other, auths); got == nil || got.ID != "auth-a" {
		t.Fatalf("control cold pick = %v, want auth-a", got)
	}
}

func TestFileTokenStoreListIgnoresStateFiles(t *testing.T) {
	dir := t.TempDir()
	writeSentinel(t, filepath.Join(dir, ".cliproxy-state", "session-affinity.state"))
	writeSentinel(t, filepath.Join(dir, ".cliproxy-state", "claude-web-search-results.state"))
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	auths, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 0 {
		t.Fatalf("state files listed as auths: %d", len(auths))
	}
}
