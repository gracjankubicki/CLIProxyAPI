package management

// Ten plik istnieje, bo plugin codex-token-usage trzyma blokady kont Codex we
// wlasnej bazie SQLite i nie implementuje interfejsu QuotaProvider, przez ktory
// rdzen moglby poprosic go o ich zdjecie. Panel wystawia przycisk
// "Clear local block", ktory bez tego endpointu konczyl sie pozornym sukcesem:
// czyscil cooldown rdzenia, podczas gdy realna blokada zyla dalej w pluginie.
//
// Zaleznosc jest swiadomie jednostronna i krucha. Rdzen zna tu nazwe tabeli i
// kolumn nalezacych do pluginu w wersji 0.1.43 i moze przestac je rozpoznawac
// po jego aktualizacji. Dlatego kazda sciezka bledu nazywa przyczyne wprost:
// po zmianie schematu awaria ma byc natychmiast widoczna, a nie objawiac sie
// cichym brakiem efektu, od ktorego zaczela sie cala ta historia.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	codexAutobanPluginID    = "codex-token-usage"
	codexAutobanTable       = "autoban_bans"
	codexAutobanReleaseNote = "manual clear via management panel"
	codexAutobanTimeout     = 15 * time.Second
)

// codexAutobanIndexPattern ogranicza wartosc trafiajaca do zapytania do
// znakow uzywanych przez identyfikatory poswiadczen. Sam indeks pochodzi juz z
// rejestru rdzenia, ale zapytanie jest skladane tekstowo, wiec wzorzec pelni
// role drugiej bariery obok podwojenia apostrofow.
var codexAutobanIndexPattern = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,128}$`)

// codexAutobanRequiredColumns to kolumny, bez ktorych nie da sie bezpiecznie
// oznaczyc blokady jako zdjetej.
var codexAutobanRequiredColumns = []string{"auth_index", "active", "released_at", "release_reason"}

// ClearCodexAutoban dezaktywuje lokalna blokade konta Codex i przy okazji
// czysci cooldown rdzenia, zeby jedno klikniecie w panelu odblokowalo konto w
// obu miejscach naraz.
func (h *Handler) ClearCodexAutoban(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	auth.EnsureIndex()

	// Od tego miejsca uzywamy indeksu z rejestru, a nie surowego wejscia.
	resolvedIndex := strings.TrimSpace(auth.Index)
	if !codexAutobanIndexPattern.MatchString(resolvedIndex) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index has an unsupported format"})
		return
	}

	databasePath, errDatabase := codexAutobanDatabasePath(h.cfg.AuthDir)
	if errDatabase != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errDatabase.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), codexAutobanTimeout)
	defer cancel()

	cleared, errClear := clearCodexAutobanEntry(ctx, databasePath, resolvedIndex)
	if errClear != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": errClear.Error()})
		return
	}

	// Cooldown rdzenia czyscimy dopiero po udanym zapisie do pluginu, zeby nie
	// rozjechac obu stanow, gdy plugin odmowi.
	updated, models, errReset := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
	if errReset != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":          fmt.Sprintf("plugin block cleared but core quota reset failed: %v", errReset),
			"cleared_blocks": cleared,
		})
		return
	}
	if updated != nil {
		updated.EnsureIndex()
		resolvedIndex = updated.Index
	}

	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"auth_index":     resolvedIndex,
		"cleared_blocks": cleared,
		"models":         models,
	})
}

// codexAutobanDatabasePath wskazuje prywatna baze pluginu wewnatrz katalogu
// poswiadczen.
func codexAutobanDatabasePath(authDir string) (string, error) {
	resolvedAuthDir, errResolve := util.ResolveAuthDir(authDir)
	if errResolve != nil {
		return "", fmt.Errorf("resolve auth dir: %w", errResolve)
	}
	databasePath := filepath.Join(resolvedAuthDir, "plugins", codexAutobanPluginID, "usage.db")
	info, errStat := os.Stat(databasePath)
	if errStat != nil {
		return "", fmt.Errorf("plugin %s database is unavailable: %s", codexAutobanPluginID, databasePath)
	}
	if info.IsDir() {
		return "", fmt.Errorf("plugin %s database path is a directory: %s", codexAutobanPluginID, databasePath)
	}
	return databasePath, nil
}

// clearCodexAutobanEntry oznacza aktywna blokade jako zdjeta i zwraca liczbe
// zmienionych wpisow. Schemat jest sprawdzany przed zapisem, zeby zmiana po
// stronie pluginu konczyla sie jawnym bledem zamiast modyfikacji czegos innego.
func clearCodexAutobanEntry(ctx context.Context, databasePath, authIndex string) (int, error) {
	if errSchema := assertCodexAutobanSchema(ctx, databasePath); errSchema != nil {
		return 0, errSchema
	}

	quotedIndex := quoteSQLiteLiteral(authIndex)
	statement := fmt.Sprintf(
		"UPDATE %s SET active = 0, released_at = strftime('%%s','now'), release_reason = %s "+
			"WHERE active = 1 AND auth_index = %s; SELECT changes();",
		codexAutobanTable,
		quoteSQLiteLiteral(codexAutobanReleaseNote),
		quotedIndex,
	)

	output, errRun := runSQLite(ctx, databasePath, statement)
	if errRun != nil {
		return 0, errRun
	}

	cleared, errParse := strconv.Atoi(strings.TrimSpace(output))
	if errParse != nil {
		return 0, fmt.Errorf("plugin %s returned an unreadable update result", codexAutobanPluginID)
	}
	if cleared == 0 {
		return 0, fmt.Errorf("no active local block found for this credential")
	}
	return cleared, nil
}

// assertCodexAutobanSchema potwierdza, ze tabela i kolumny nadal wygladaja tak,
// jak zaklada ten endpoint.
func assertCodexAutobanSchema(ctx context.Context, databasePath string) error {
	tableOutput, errTable := runSQLite(ctx, databasePath,
		fmt.Sprintf("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = %s;",
			quoteSQLiteLiteral(codexAutobanTable)))
	if errTable != nil {
		return errTable
	}
	if strings.TrimSpace(tableOutput) != "1" {
		return fmt.Errorf("plugin %s no longer exposes the %s table; the panel action needs an update",
			codexAutobanPluginID, codexAutobanTable)
	}

	columnOutput, errColumns := runSQLite(ctx, databasePath,
		fmt.Sprintf("SELECT name FROM pragma_table_info(%s);", quoteSQLiteLiteral(codexAutobanTable)))
	if errColumns != nil {
		return errColumns
	}
	available := make(map[string]bool)
	for _, line := range strings.Split(columnOutput, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			available[trimmed] = true
		}
	}
	for _, column := range codexAutobanRequiredColumns {
		if !available[column] {
			return fmt.Errorf("plugin %s changed the %s schema; missing column %q",
				codexAutobanPluginID, codexAutobanTable, column)
		}
	}
	return nil
}

// runSQLite wykonuje pojedyncze zapytanie przy uzyciu klienta sqlite3 z obrazu.
// Rdzen nie ma sterownika SQLite, a dokladanie go rozszerzyloby lokalny patch o
// zaleznosci w go.mod, ktore kolidowalyby przy kazdym upgradzie upstreamu.
func runSQLite(ctx context.Context, databasePath, statement string) (string, error) {
	command := exec.CommandContext(ctx, "sqlite3", "-batch", "-noheader", databasePath, statement)
	output, errRun := command.CombinedOutput()
	if errRun != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("plugin %s database did not respond in time", codexAutobanPluginID)
		}
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = errRun.Error()
		}
		return "", fmt.Errorf("plugin %s database query failed: %s", codexAutobanPluginID, detail)
	}
	return string(output), nil
}

// quoteSQLiteLiteral zamyka wartosc w literale tekstowym, podwajajac apostrofy.
func quoteSQLiteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
