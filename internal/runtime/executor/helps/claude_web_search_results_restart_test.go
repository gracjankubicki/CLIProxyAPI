package helps

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/cache"
)

// The request sent upstream after a graceful restart must carry the exact
// results Claude returned before the restart.
func TestClaudeWebSearchResultsSurviveSimulatedRestart(t *testing.T) {
	resetClaudeWebSearchResults(t)
	path := filepath.Join(t.TempDir(), ".cliproxy-state", "claude-web-search-results.state")

	writer := cache.EnableClaudeWebSearchResultPersistence(path, 16)
	if writer == nil {
		t.Fatal("persistence not enabled")
	}
	event := []byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":` + claudeWebSearchTestResults + `}}`)
	RecordClaudeWebSearchResults("session-1", event, 30*time.Minute, 16)
	if err := writer.SealAndFlush(); err != nil {
		t.Fatal(err)
	}

	// Process exit: memory is gone, the next process loads the file.
	cache.ClearClaudeWebSearchResultCache()
	if got := RestoreClaudeWebSearchResults(claudeWebSearchRequestBody(`[]`), "session-1", 30*time.Minute); ojson.GetBytes(got, "messages.1.content.1.content").Raw != `[]` {
		t.Fatal("precondition: memory should be empty before reload")
	}
	next := cache.EnableClaudeWebSearchResultPersistence(path, 16)
	if next == nil {
		t.Fatal("persistence disabled after restart")
	}
	t.Cleanup(func() { _ = next.SealAndFlush() })

	got := RestoreClaudeWebSearchResultsContext(context.Background(), claudeWebSearchRequestBody(`[]`), "session-1", 30*time.Minute)
	if raw := ojson.GetBytes(got, "messages.1.content.1.content").Raw; raw != claudeWebSearchTestResults {
		t.Fatalf("restored after restart = %s, want byte-identical original", raw)
	}
	if other := RestoreClaudeWebSearchResults(claudeWebSearchRequestBody(`[]`), "session-2", 30*time.Minute); ojson.GetBytes(other, "messages.1.content.1.content").Raw != `[]` {
		t.Fatal("results leaked to another session after restart")
	}
}
