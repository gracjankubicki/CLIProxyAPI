package cliproxy

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestLocalStatePersistenceWarnsWhenReloadAsksForDifferentFiles(t *testing.T) {
	hook := logtest.NewGlobal()
	t.Cleanup(hook.Reset)

	startup := persistenceConfig("/auth")
	startup.Client.Codex.WebSearchResultCache.Enabled = false // off at startup
	p := &localStatePersistence{}
	p.webSearchPath, p.affinityPath = desiredStatePaths(startup, true)
	p.warnedFor = p.webSearchPath + "\x00" + p.affinityPath

	warnings := func() []string {
		var out []string
		for _, entry := range hook.AllEntries() {
			if entry.Level == log.WarnLevel {
				out = append(out, entry.Message)
			}
		}
		return out
	}

	// Enabling the web search cache at runtime: its file is not used until restart.
	p.warnOnReload(persistenceConfig("/auth"), true)
	if got := warnings(); len(got) != 1 || !strings.Contains(got[0], "claude web search results") {
		t.Fatalf("enabling the cache at runtime: warnings = %v", got)
	}
	// Repeating the same reload does not repeat the warning.
	p.warnOnReload(persistenceConfig("/auth"), true)
	if got := warnings(); len(got) != 1 {
		t.Fatalf("repeated reload warned again: %v", got)
	}
	// auth-dir change moves the affinity file: warn about it.
	p.warnOnReload(persistenceConfig("/other-auth"), true)
	got := warnings()
	if len(got) != 3 || !strings.Contains(got[2], "session-affinity") {
		t.Fatalf("auth-dir change: warnings = %v", got)
	}
	// Going back to the startup decision clears the mismatch silently.
	hook.Reset()
	p.warnOnReload(startup, true)
	if got := warnings(); len(got) != 0 {
		t.Fatalf("returning to startup config: warnings = %v", got)
	}
}
