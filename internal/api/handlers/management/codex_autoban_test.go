package management

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newCodexAutobanFixture buduje minimalna kopie tabeli pluginu, zeby testy nie
// zalezaly od prawdziwej bazy runtime.
func newCodexAutobanFixture(t *testing.T, seed string) string {
	t.Helper()
	if _, errLook := exec.LookPath("sqlite3"); errLook != nil {
		t.Skip("sqlite3 client is unavailable")
	}

	authDir := t.TempDir()
	databaseDir := filepath.Join(authDir, "plugins", codexAutobanPluginID)
	if errMkdir := os.MkdirAll(databaseDir, 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	databasePath := filepath.Join(databaseDir, "usage.db")
	command := exec.Command("sqlite3", "-batch", databasePath, seed)
	if output, errRun := command.CombinedOutput(); errRun != nil {
		t.Fatalf("seed database: %v: %s", errRun, output)
	}
	return authDir
}

const codexAutobanSchemaSeed = `CREATE TABLE autoban_bans (
  auth_id TEXT PRIMARY KEY,
  auth_index TEXT NOT NULL DEFAULT '',
  active INTEGER NOT NULL DEFAULT 1,
  released_at INTEGER NOT NULL DEFAULT 0,
  release_reason TEXT NOT NULL DEFAULT ''
);
INSERT INTO autoban_bans (auth_id, auth_index, active) VALUES ('codex-one.json', 'abcdef1234567890', 1);
INSERT INTO autoban_bans (auth_id, auth_index, active) VALUES ('codex-two.json', 'fedcba0987654321', 0);`

func TestClearCodexAutobanEntryDeactivatesOnlyMatchingActiveBlock(t *testing.T) {
	authDir := newCodexAutobanFixture(t, codexAutobanSchemaSeed)
	databasePath, errPath := codexAutobanDatabasePath(authDir)
	if errPath != nil {
		t.Fatal(errPath)
	}

	cleared, errClear := clearCodexAutobanEntry(context.Background(), databasePath, "abcdef1234567890")
	if errClear != nil {
		t.Fatalf("clear returned %v", errClear)
	}
	if cleared != 1 {
		t.Fatalf("cleared = %d, want 1", cleared)
	}

	output, errQuery := runSQLite(context.Background(), databasePath,
		"SELECT auth_id || '=' || active || '|' || release_reason FROM autoban_bans ORDER BY auth_id;")
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	if !strings.Contains(output, "codex-one.json=0|"+codexAutobanReleaseNote) {
		t.Fatalf("target row was not released: %s", output)
	}
	// Sasiedni wpis nie moze zostac dotkniety, nawet gdy jest juz nieaktywny.
	if !strings.Contains(output, "codex-two.json=0|") || strings.Count(output, codexAutobanReleaseNote) != 1 {
		t.Fatalf("unrelated row changed: %s", output)
	}
}

func TestClearCodexAutobanEntryReportsMissingActiveBlock(t *testing.T) {
	authDir := newCodexAutobanFixture(t, codexAutobanSchemaSeed)
	databasePath, errPath := codexAutobanDatabasePath(authDir)
	if errPath != nil {
		t.Fatal(errPath)
	}

	// Wpis istnieje, ale jest juz zdjety: panel ma zobaczyc blad, nie sukces.
	if _, errClear := clearCodexAutobanEntry(context.Background(), databasePath, "fedcba0987654321"); errClear == nil {
		t.Fatal("expected an error for an already released block")
	}
	if _, errClear := clearCodexAutobanEntry(context.Background(), databasePath, "0000000000000000"); errClear == nil {
		t.Fatal("expected an error for an unknown credential")
	}
}

func TestClearCodexAutobanEntryRejectsChangedSchema(t *testing.T) {
	renamedTable := `CREATE TABLE autoban_bans_v2 (auth_index TEXT, active INTEGER);`
	authDir := newCodexAutobanFixture(t, renamedTable)
	databasePath, errPath := codexAutobanDatabasePath(authDir)
	if errPath != nil {
		t.Fatal(errPath)
	}

	_, errClear := clearCodexAutobanEntry(context.Background(), databasePath, "abcdef1234567890")
	if errClear == nil {
		t.Fatal("expected an error when the table is gone")
	}
	if !strings.Contains(errClear.Error(), "no longer exposes") {
		t.Fatalf("error should name the cause, got %v", errClear)
	}

	missingColumn := `CREATE TABLE autoban_bans (auth_index TEXT, active INTEGER);`
	authDirPartial := newCodexAutobanFixture(t, missingColumn)
	databasePathPartial, errPathPartial := codexAutobanDatabasePath(authDirPartial)
	if errPathPartial != nil {
		t.Fatal(errPathPartial)
	}
	_, errPartial := clearCodexAutobanEntry(context.Background(), databasePathPartial, "abcdef1234567890")
	if errPartial == nil {
		t.Fatal("expected an error when a required column is missing")
	}
	if !strings.Contains(errPartial.Error(), "missing column") {
		t.Fatalf("error should name the missing column, got %v", errPartial)
	}
}

func TestCodexAutobanDatabasePathReportsMissingDatabase(t *testing.T) {
	if _, errPath := codexAutobanDatabasePath(t.TempDir()); errPath == nil {
		t.Fatal("expected an error when the plugin database is absent")
	}
}

// Indeks trafia do zapytania jako literal, wiec probe zamkniecia literalu i
// dopisania wlasnego SQL musza pozostac nieszkodliwe.
func TestCodexAutobanRejectsInjectionAttempts(t *testing.T) {
	for _, candidate := range []string{
		"abc' OR '1'='1",
		"abc'; DROP TABLE autoban_bans; --",
		"abc def",
		"",
		strings.Repeat("a", 129),
	} {
		if codexAutobanIndexPattern.MatchString(candidate) {
			t.Fatalf("pattern accepted unsafe auth_index %q", candidate)
		}
	}
	for _, candidate := range []string{"abcdef1234567890", "claude:oauth:1", "codex-de5f1699.json"} {
		if !codexAutobanIndexPattern.MatchString(candidate) {
			t.Fatalf("pattern rejected a legitimate auth_index %q", candidate)
		}
	}

	authDir := newCodexAutobanFixture(t, codexAutobanSchemaSeed)
	databasePath, errPath := codexAutobanDatabasePath(authDir)
	if errPath != nil {
		t.Fatal(errPath)
	}
	// Nawet gdyby wzorzec zostal kiedys poluzowany, podwojenie apostrofow musi
	// utrzymac wartosc w granicach literalu i zostawic tabele nietknieta.
	if _, errClear := clearCodexAutobanEntry(context.Background(), databasePath,
		"abc'; DROP TABLE autoban_bans; --"); errClear == nil {
		t.Fatal("expected no rows to match the injected value")
	}
	output, errQuery := runSQLite(context.Background(), databasePath, "SELECT COUNT(*) FROM autoban_bans;")
	if errQuery != nil {
		t.Fatal(errQuery)
	}
	if strings.TrimSpace(output) != "2" {
		t.Fatalf("table was altered by the injected value: %s", output)
	}
}

func TestQuoteSQLiteLiteralDoublesQuotes(t *testing.T) {
	if got := quoteSQLiteLiteral("O'Reilly"); got != "'O''Reilly'" {
		t.Fatalf("quoted = %s", got)
	}
}
