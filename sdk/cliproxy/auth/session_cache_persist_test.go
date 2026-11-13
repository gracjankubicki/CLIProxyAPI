package auth

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/statefile"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func reopenSessionCache(t *testing.T, cache *SessionCache, path string, ttl time.Duration) *SessionCache {
	t.Helper()
	if cache.writer != nil {
		if err := cache.writer.SealAndFlush(); err != nil {
			t.Fatalf("seal: %v", err)
		}
	}
	cache.Stop()
	next := NewSessionCache(ttl)
	t.Cleanup(next.Stop)
	if next.EnablePersistence(path) == nil {
		t.Fatal("persistence disabled after restart")
	}
	t.Cleanup(func() { _ = next.writer.SealAndFlush() })
	return next
}

func TestSessionCachePersistenceRoundTripKeepsAliasesAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cliproxy-state", "session-affinity.state")
	cache := NewSessionCache(time.Hour)
	if cache.EnablePersistence(path) == nil {
		t.Fatal("persistence not enabled")
	}
	cache.SetAliases("auth-a.json", "mixed::codex:s1::m", "mixed::pck:s1::m")
	cache.Set("mixed::codex:s2::m", "auth-b.json")
	if !cache.writer.Dirty() {
		t.Fatal("binding not marked for persistence")
	}
	cache.mu.RLock()
	wantExpiry := cache.entries["mixed::codex:s1::m"].expiresAt
	cache.mu.RUnlock()

	next := reopenSessionCache(t, cache, path, time.Hour)
	for alias, want := range map[string]string{
		"mixed::codex:s1::m": "auth-a.json",
		"mixed::pck:s1::m":   "auth-a.json",
		"mixed::codex:s2::m": "auth-b.json",
	} {
		if got, ok := next.Get(alias); !ok || got != want {
			t.Fatalf("Get(%s) = %q, %v; want %q", alias, got, ok, want)
		}
	}
	next.mu.RLock()
	gotExpiry := next.entries["mixed::codex:s1::m"].expiresAt
	next.mu.RUnlock()
	if !gotExpiry.Equal(wantExpiry) {
		t.Fatalf("load refreshed expiry: got %v want %v", gotExpiry, wantExpiry)
	}
	if next.writer.Dirty() {
		t.Fatal("clean load must not schedule a write")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestSessionCachePersistenceDropsExpiredAndAppliesCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.state")
	cache := NewSessionCacheWithCapacity(time.Hour, 8)
	cache.EnablePersistence(path)
	cache.mu.Lock()
	cache.replaceAliasGroupsLocked("auth-old", time.Now().Add(-time.Minute), []string{"expired"}, sessionBinding{})
	cache.mu.Unlock()
	cache.Set("first", "auth-1")
	cache.Set("second", "auth-2")
	cache.Set("third", "auth-3")
	if err := cache.writer.SealAndFlush(); err != nil {
		t.Fatal(err)
	}
	cache.Stop()

	next := NewSessionCacheWithCapacity(time.Hour, 2)
	defer next.Stop()
	writer := next.EnablePersistence(path)
	defer func() { _ = writer.SealAndFlush() }()
	if _, ok := next.Get("expired"); ok {
		t.Fatal("expired binding loaded")
	}
	if _, ok := next.Get("first"); ok {
		t.Fatal("oldest binding kept beyond capacity")
	}
	for _, id := range []string{"second", "third"} {
		if _, ok := next.Get(id); !ok {
			t.Fatalf("fresh binding %q lost", id)
		}
	}
	if !writer.Dirty() {
		t.Fatal("skipped expired entry must schedule a rewrite")
	}
}

func TestSessionCachePersistenceMarksEveryMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.state")
	cache := NewSessionCache(time.Hour)
	cache.EnablePersistence(path)
	cache.SetAliases("auth-a", "s1", "s1-alt")
	cache.Set("s2", "auth-b")
	cases := []struct {
		name string
		do   func(c *SessionCache)
	}{
		{"GetAndRefresh", func(c *SessionCache) { c.GetAndRefresh("s1") }},
		{"Touch", func(c *SessionCache) { c.Touch("s1", "auth-a") }},
		{"CompareAndDelete", func(c *SessionCache) { c.CompareAndDelete("s1-alt", "auth-a") }},
		{"Invalidate", func(c *SessionCache) { c.Invalidate("s1") }},
		{"InvalidateAuth", func(c *SessionCache) { c.InvalidateAuth("auth-b") }},
		{"Set", func(c *SessionCache) { c.Set("s3", "auth-c") }},
	}
	for _, tc := range cases {
		cache = reopenSessionCache(t, cache, path, time.Hour)
		if cache.writer.Dirty() {
			t.Fatalf("%s: dirty before change", tc.name)
		}
		tc.do(cache)
		if !cache.writer.Dirty() {
			t.Fatalf("%s: mutation not marked for persistence", tc.name)
		}
	}

	// Background cleanup and capacity eviction remove groups without a caller.
	plain := NewSessionCacheWithCapacity(time.Hour, 1)
	defer plain.Stop()
	plain.mu.Lock()
	plain.replaceAliasGroupsLocked("auth-x", time.Now().Add(-time.Second), []string{"gone"}, sessionBinding{})
	plain.mu.Unlock()
	plain.writer = statefile.NewWriter("test", filepath.Join(t.TempDir(), "c.state"), &plain.mu, plain.snapshotState)
	defer func() { _ = plain.writer.SealAndFlush() }()
	plain.cleanup()
	if !plain.writer.Dirty() {
		t.Fatal("cleanup not marked for persistence")
	}
	_ = plain.writer.SealAndFlush()
	plain.writer = statefile.NewWriter("test", filepath.Join(t.TempDir(), "d.state"), &plain.mu, plain.snapshotState)
	plain.Set("a", "auth-a")
	plain.Set("b", "auth-b")
	if _, ok := plain.Get("a"); ok {
		t.Fatal("capacity eviction did not happen")
	}
}

func TestSessionAffinitySelectorRestoresBindingAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-affinity.state")
	auths := []*Auth{{ID: "auth-a"}, {ID: "auth-b"}}
	headers := http.Header{}
	headers.Set("Session-Id", "019a0000-1111-2222-3333-444455556666")
	opts := cliproxyexecutor.Options{Headers: headers, OriginalRequest: []byte(`{"input":"hi"}`)}

	pickWith := func(cache *SessionCache, fallback Selector) string {
		selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback, TTL: time.Hour, Cache: cache})
		defer selector.Stop()
		got, err := selector.Pick(context.Background(), "mixed", "gpt-6.1-sol", opts, auths)
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		return got.ID
	}

	cache := NewSessionCache(time.Hour)
	cache.EnablePersistence(path)
	// Round-robin hands the second request to the other account; the binding must win.
	rr := &RoundRobinSelector{}
	first := pickWith(cache, rr)
	if again := pickWith(cache, rr); again != first {
		t.Fatalf("shared cache lost binding across selector rebuild: %s -> %s", first, again)
	}
	if cache.Len() == 0 {
		t.Fatal("selector Stop cleared the shared cache")
	}

	restarted := reopenSessionCache(t, cache, path, time.Hour)
	freshRR := &RoundRobinSelector{}
	// Advance the fresh round-robin so a cold pick would choose the other account.
	if first == "auth-a" {
		_, _ = freshRR.Pick(context.Background(), "mixed", "gpt-6.1-sol", cliproxyexecutor.Options{}, auths)
	}
	if got := pickWith(restarted, freshRR); got != first {
		t.Fatalf("after restart picked %s, want bound %s", got, first)
	}
}

func TestSessionAffinitySelectorSharedCacheGetsCurrentTTL(t *testing.T) {
	cache := NewSessionCache(time.Hour)
	defer cache.Stop()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{TTL: 2 * time.Hour, Cache: cache})
	selector.Stop()
	cache.mu.RLock()
	ttl := cache.ttl
	cache.mu.RUnlock()
	if ttl != 2*time.Hour {
		t.Fatalf("shared cache ttl = %v, want 2h", ttl)
	}
	select {
	case <-cache.stopCh:
		t.Fatal("selector Stop stopped the shared cache")
	default:
	}
}

// The selection binding used to find a parent session's credential (guardian
// reviews) survives a restart, including the order of selections.
func TestSessionCachePersistenceKeepsSelectionBindingOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cliproxy-state", "session-affinity.state")
	cache := NewSessionCache(time.Hour)
	if cache.EnablePersistence(path) == nil {
		t.Fatal("persistence not enabled")
	}
	cache.setSessionAliases("auth-old.json", "codex", "parent", "mixed::codex:parent::m1")
	cache.setSessionAliases("auth-new.json", "codex", "parent", "mixed::codex:parent::m2")
	providers := map[string]struct{}{"codex": {}}
	if got := cache.latestSessionAuth("parent", providers); got != "auth-new.json" {
		t.Fatalf("before restart latestSessionAuth = %q", got)
	}

	// A TTL refresh of the older selection moves it to the end of the eviction
	// order but must not make it the newest selection.
	if !cache.Touch("mixed::codex:parent::m1", "auth-old.json") {
		t.Fatal("touch of the older selection failed")
	}
	if got := cache.latestSessionAuth("parent", providers); got != "auth-new.json" {
		t.Fatalf("after touch latestSessionAuth = %q", got)
	}

	next := reopenSessionCache(t, cache, path, time.Hour)
	if got := next.latestSessionAuth("parent", providers); got != "auth-new.json" {
		t.Fatalf("after restart latestSessionAuth = %q, want auth-new.json", got)
	}
	// A selection made after the restart is newer than every restored one.
	next.setSessionAliases("auth-third.json", "codex", "parent", "mixed::codex:parent::m3")
	if got := next.latestSessionAuth("parent", providers); got != "auth-third.json" {
		t.Fatalf("new selection after restart latestSessionAuth = %q, want auth-third.json", got)
	}
	if got := next.latestSessionAuth("parent", map[string]struct{}{"claude": {}}); got != "" {
		t.Fatalf("other provider latestSessionAuth = %q, want empty", got)
	}
}

// A state file written before bindings were stored still loads; its entries
// have no selection binding.
func TestSessionCachePersistenceLoadsFileWithoutBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cliproxy-state", "session-affinity.state")
	raw, err := statefile.Encode(sessionAffinityStateVersion, []map[string]any{
		{"auth_id": "auth-a.json", "expires_at": time.Now().Add(time.Hour), "aliases": []string{"mixed::codex:s1::m"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewSessionCache(time.Hour)
	t.Cleanup(cache.Stop)
	if cache.EnablePersistence(path) == nil {
		t.Fatal("persistence disabled for an old file")
	}
	if got, ok := cache.Get("mixed::codex:s1::m"); !ok || got != "auth-a.json" {
		t.Fatalf("Get = %q, %v", got, ok)
	}
	if got := cache.latestSessionAuth("s1", map[string]struct{}{"codex": {}}); got != "" {
		t.Fatalf("latestSessionAuth = %q, want empty for a record without binding", got)
	}
}
