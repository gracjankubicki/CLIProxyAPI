package cache

import (
	"github.com/MoeForge/moego/json"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/statefile"
	log "github.com/sirupsen/logrus"
)

const claudeWebSearchResultStateVersion = 1

// claudeWebSearchResultWriter is set once at startup when file persistence is
// active. It is read and used under claudeWebSearchResultMu.
var claudeWebSearchResultWriter *statefile.Writer

type claudeWebSearchResultRecord struct {
	Key       string          `json:"key"`
	Content   json.RawMessage `json:"content"`
	TTL       int64           `json:"ttl_ns"`
	ExpiresAt time.Time       `json:"expires_at"`
}

func markClaudeWebSearchResultDirtyLocked() {
	if claudeWebSearchResultWriter != nil {
		claudeWebSearchResultWriter.MarkDirty()
	}
}

// EnableClaudeWebSearchResultPersistence loads path into the web search result
// cache and keeps the file updated afterwards. It must be called once at
// startup, before requests are served. The returned writer is nil when
// persistence is disabled because of an unsupported or unreadable file.
func EnableClaudeWebSearchResultPersistence(path string, limit int) *statefile.Writer {
	if path == "" {
		return nil
	}
	if limit <= 0 {
		limit = defaultClaudeWebSearchResultLimit
	}
	now := claudeWebSearchResultNow()
	raw, status := statefile.Load(path, claudeWebSearchResultStateVersion, now)
	if !status.Active() {
		return nil
	}

	claudeWebSearchResultMu.Lock()
	defer claudeWebSearchResultMu.Unlock()

	loaded, skipped := 0, 0
	// Records are stored most recently used first; append keeps that order.
	for _, item := range raw {
		var record claudeWebSearchResultRecord
		if errDecode := json.Unmarshal(item, &record); errDecode != nil || len(record.Key) != 64 ||
			len(record.Content) == 0 || !json.Valid(record.Content) || record.TTL <= 0 {
			skipped++
			continue
		}
		if !now.Before(record.ExpiresAt) {
			skipped++
			continue
		}
		if _, exists := claudeWebSearchResultIndex[record.Key]; exists {
			skipped++
			continue
		}
		if claudeWebSearchResultOrder.Len() >= limit {
			skipped++
			continue
		}
		entry := &claudeWebSearchResultEntry{
			key:       record.Key,
			content:   append([]byte(nil), record.Content...),
			ttl:       time.Duration(record.TTL),
			expiresAt: record.ExpiresAt,
		}
		claudeWebSearchResultIndex[record.Key] = claudeWebSearchResultOrder.PushBack(entry)
		loaded++
	}
	if loaded > 0 || skipped > 0 {
		cacheCleanupOnce.Do(startCacheCleanup)
	}
	writer := statefile.NewWriter("claude-web-search-results", path, &claudeWebSearchResultMu, snapshotClaudeWebSearchResults)
	claudeWebSearchResultWriter = writer
	if skipped > 0 || status == statefile.LoadQuarantined {
		writer.MarkDirty()
	}
	log.Infof("claude web search results: persistence enabled | file=%s loaded=%d skipped=%d", path, loaded, skipped)
	return writer
}

// snapshotClaudeWebSearchResults runs with claudeWebSearchResultMu held by the writer.
func snapshotClaudeWebSearchResults() ([]byte, error) {
	records := make([]claudeWebSearchResultRecord, 0, claudeWebSearchResultOrder.Len())
	for element := claudeWebSearchResultOrder.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*claudeWebSearchResultEntry)
		records = append(records, claudeWebSearchResultRecord{
			Key:       entry.key,
			Content:   json.RawMessage(append([]byte(nil), entry.content...)),
			TTL:       int64(entry.ttl),
			ExpiresAt: entry.expiresAt,
		})
	}
	return statefile.Encode(claudeWebSearchResultStateVersion, records)
}

// disableClaudeWebSearchResultPersistenceForTest seals and detaches the writer
// so no background write outlives a test.
func disableClaudeWebSearchResultPersistenceForTest() {
	claudeWebSearchResultMu.Lock()
	writer := claudeWebSearchResultWriter
	claudeWebSearchResultWriter = nil
	claudeWebSearchResultMu.Unlock()
	if writer != nil {
		_ = writer.SealAndFlush()
	}
}
