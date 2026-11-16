package management

// The codex-token-usage plugin stores account blocks in its own SQLite database
// and has no QuotaProvider interface for clearing them. Without this endpoint,
// the panel could clear the core cooldown while leaving the actual plugin block
// in place. This integration depends on the plugin 0.1.43 schema; schema changes
// must fail explicitly rather than report success without removing the block.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/regexp"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

const (
	codexAutobanPluginID    = "codex-token-usage"
	codexAutobanTable       = "autoban_bans"
	codexAutobanReleaseNote = "manual clear via management panel"
	codexAutobanTimeout     = 15 * time.Second
)

// codexAutobanIndexPattern restricts query input to credential identifier
// characters. Registry lookup and SQL literal escaping provide separate guards.
var codexAutobanIndexPattern = regexp.MustCompile(`^[A-Za-z0-9_:.-]{1,128}$`)

// codexAutobanRequiredColumns are needed to safely mark a block as released.
var codexAutobanRequiredColumns = []string{"auth_index", "active", "released_at", "release_reason"}

// ClearCodexAutoban releases a local Codex account block and clears the core
// cooldown so the panel action updates both stores.
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

	// Use the registry index from this point onward.
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

	// Clear the core cooldown only after the plugin database update succeeds.
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

// codexAutobanDatabasePath locates the private plugin database under auth-dir.
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

// clearCodexAutobanEntry releases an active block and returns the changed row
// count. Validate the schema before writing to detect incompatible plugin updates.
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

// assertCodexAutobanSchema validates the table and columns this endpoint needs.
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

// runSQLite executes one query with the image's sqlite3 CLI, avoiding an extra
// Go driver dependency in the local patch set.
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

// quoteSQLiteLiteral escapes a SQL text literal by doubling single quotes.
func quoteSQLiteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
