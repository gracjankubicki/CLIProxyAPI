package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/statefile"
)

func usePersistedWebSearchCache(t *testing.T, now *time.Time) string {
	t.Helper()
	useClaudeWebSearchResultClock(t, now)
	t.Cleanup(disableClaudeWebSearchResultPersistenceForTest)
	return filepath.Join(t.TempDir(), ".cliproxy-state", "claude-web-search-results.state")
}

// simulateRestart seals the current writer, drops in-memory state the way a
// process exit would, and loads the file into a fresh cache.
func simulateWebSearchRestart(t *testing.T, writer *statefile.Writer, path string, limit int) *statefile.Writer {
	t.Helper()
	if err := writer.SealAndFlush(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	disableClaudeWebSearchResultPersistenceForTest()
	ClearClaudeWebSearchResultCache()
	next := EnableClaudeWebSearchResultPersistence(path, limit)
	if next == nil {
		t.Fatal("persistence disabled after restart")
	}
	return next
}

func TestClaudeWebSearchResultPersistenceRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := usePersistedWebSearchCache(t, &now)

	writer := EnableClaudeWebSearchResultPersistence(path, 16)
	if writer == nil {
		t.Fatal("persistence not enabled for a missing file")
	}
	content := `[{"type":"web_search_result","url":"https://example.com","title":"Zażółć","encrypted_content":"abc<>&"}]`
	CacheClaudeWebSearchResult("session-a", "srvtoolu_1", []byte(content), time.Hour, 16)
	CacheClaudeWebSearchResult("session-b", "srvtoolu_2", []byte(`[2]`), time.Hour, 16)
	if !writer.Dirty() {
		t.Fatal("insert did not mark the state dirty")
	}

	simulateWebSearchRestart(t, writer, path, 16)
	got, ok := GetClaudeWebSearchResult("session-a", "srvtoolu_1")
	if !ok || string(got) != content {
		t.Fatalf("restored = %q, %v; want byte-identical content", got, ok)
	}
	if _, ok := GetClaudeWebSearchResult("session-b", "srvtoolu_1"); ok {
		t.Fatal("content leaked across sessions after restart")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, err=%v", info.Mode().Perm(), err)
	}
	if !strings.HasSuffix(path, ".state") {
		t.Fatal("state file must not use the .json auth extension")
	}
}

func TestClaudeWebSearchResultPersistenceDropsExpiredAndKeepsLRUWithinLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := usePersistedWebSearchCache(t, &now)
	writer := EnableClaudeWebSearchResultPersistence(path, 16)

	CacheClaudeWebSearchResult("s", "old", []byte(`["old"]`), time.Hour, 16)
	CacheClaudeWebSearchResult("s", "short", []byte(`["short"]`), time.Minute, 16)
	CacheClaudeWebSearchResult("s", "mid", []byte(`["mid"]`), time.Hour, 16)
	CacheClaudeWebSearchResult("s", "new", []byte(`["new"]`), time.Hour, 16)
	now = now.Add(2 * time.Minute)
	// Reading "old" makes it the most recently used entry.
	if _, ok := GetClaudeWebSearchResult("s", "old"); !ok {
		t.Fatal("old entry missing")
	}

	simulateWebSearchRestart(t, writer, path, 2)
	if claudeWebSearchResultOrder.Len() != 2 {
		t.Fatalf("loaded %d entries, want limit 2", claudeWebSearchResultOrder.Len())
	}
	for _, id := range []string{"old", "new"} {
		if _, ok := GetClaudeWebSearchResult("s", id); !ok {
			t.Fatalf("most recently used entry %q not kept", id)
		}
	}
	for _, id := range []string{"short", "mid"} {
		if _, ok := GetClaudeWebSearchResult("s", id); ok {
			t.Fatalf("entry %q should be expired or beyond the limit", id)
		}
	}
}

func TestClaudeWebSearchResultPersistenceKeepsStoredExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := usePersistedWebSearchCache(t, &now)
	writer := EnableClaudeWebSearchResultPersistence(path, 16)
	CacheClaudeWebSearchResult("s", "a", []byte(`[1]`), time.Hour, 16)
	now = now.Add(50 * time.Minute)
	writer = simulateWebSearchRestart(t, writer, path, 16)
	now = now.Add(11 * time.Minute)
	if _, ok := GetClaudeWebSearchResult("s", "a"); ok {
		t.Fatal("load must not extend the stored expiry")
	}
	_ = writer
}

func TestClaudeWebSearchResultPersistenceMarksRefreshEvictExpireAndClear(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := usePersistedWebSearchCache(t, &now)
	writer := EnableClaudeWebSearchResultPersistence(path, 16)
	CacheClaudeWebSearchResult("s", "a", []byte(`[1]`), time.Hour, 1)
	reopen := func() {
		writer = simulateWebSearchRestart(t, writer, path, 16)
	}

	cases := []struct {
		name string
		do   func()
	}{
		{"refresh", func() { GetClaudeWebSearchResult("s", "a") }},
		{"evict", func() { CacheClaudeWebSearchResult("s", "b", []byte(`[2]`), time.Minute, 1) }},
		{"expire", func() {
			now = now.Add(2 * time.Minute)
			purgeExpiredClaudeWebSearchResultCache(now)
		}},
		{"clear", ClearClaudeWebSearchResultCache},
	}
	for _, tc := range cases {
		if tc.name == "clear" {
			CacheClaudeWebSearchResult("s", "c", []byte(`[3]`), time.Hour, 16)
		}
		reopen()
		if writer.Dirty() {
			t.Fatalf("%s: writer dirty before the change", tc.name)
		}
		tc.do()
		if !writer.Dirty() {
			t.Fatalf("%s: change not marked for persistence", tc.name)
		}
	}
}

func TestClaudeWebSearchResultPersistenceFileProblems(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	path := usePersistedWebSearchCache(t, &now)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	// Unknown version: untouched and persistence disabled.
	future, _ := statefile.Encode(99, []int{})
	if err := os.WriteFile(path, future, 0o600); err != nil {
		t.Fatal(err)
	}
	if EnableClaudeWebSearchResultPersistence(path, 16) != nil {
		t.Fatal("unknown version must disable persistence")
	}
	if got, _ := os.ReadFile(path); string(got) != string(future) {
		t.Fatal("unknown version file modified")
	}

	// Known version with broken entries: valid ones load, file rewritten on next write.
	good := claudeWebSearchResultRecord{Content: json.RawMessage(`[1]`), TTL: int64(time.Hour), ExpiresAt: now.Add(time.Hour)}
	good.Key, _ = claudeWebSearchResultKey("s", "a")
	data, _ := statefile.Encode(claudeWebSearchResultStateVersion, []any{good, map[string]any{"key": "short"}, "garbage"})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	writer := EnableClaudeWebSearchResultPersistence(path, 16)
	if writer == nil || !writer.Dirty() {
		t.Fatal("broken entries must keep persistence on and schedule a rewrite")
	}
	if _, ok := GetClaudeWebSearchResult("s", "a"); !ok {
		t.Fatal("valid entry not loaded next to broken ones")
	}
	disableClaudeWebSearchResultPersistenceForTest()
	ClearClaudeWebSearchResultCache()

	// Unparseable file: quarantined, start empty, persistence active.
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if EnableClaudeWebSearchResultPersistence(path, 16) == nil {
		t.Fatal("corrupt file must keep persistence active")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("quarantine files = %v", matches)
	}
}
