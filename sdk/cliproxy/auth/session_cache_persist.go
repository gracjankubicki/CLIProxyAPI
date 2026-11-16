package auth

import (
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/statefile"
	log "github.com/sirupsen/logrus"
)

const sessionAffinityStateVersion = 1

type sessionAffinityRecord struct {
	AuthID    string    `json:"auth_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Aliases   []string  `json:"aliases"`
	// Session and provider of the selection that created the binding. Files
	// written before these fields existed load with an empty binding.
	BindingSession  string `json:"binding_session,omitempty"`
	BindingProvider string `json:"binding_provider,omitempty"`
	// BindingSequence is the selection order. It is independent of the record
	// order, which follows eviction and changes on TTL refreshes.
	BindingSequence uint64 `json:"binding_sequence,omitempty"`
}

func (c *SessionCache) markDirtyLocked() {
	if c.writer != nil {
		c.writer.MarkDirty()
	}
}

// SetTTL changes the lifetime applied by later refreshes. Existing expirations
// are kept unchanged.
func (c *SessionCache) SetTTL(ttl time.Duration) {
	if c == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.ttl = ttl
	c.mu.Unlock()
}

// EnablePersistence loads bindings from path and keeps the file updated
// afterwards. Call it once at startup before the cache serves requests. Loaded
// bindings keep their stored expiration and are not refreshed. The returned
// writer is nil when persistence is disabled by an unsupported or unreadable file.
func (c *SessionCache) EnablePersistence(path string) *statefile.Writer {
	if c == nil || path == "" {
		return nil
	}
	now := time.Now()
	raw, status := statefile.Load(path, sessionAffinityStateVersion, now)
	if !status.Active() {
		return nil
	}
	writer := statefile.NewWriter("session-affinity", path, &c.mu, c.snapshotState)

	c.mu.Lock()
	c.ensureInitializedLocked()
	loaded, skipped := 0, 0
	// Records are stored oldest first, matching the eviction order.
	for _, item := range raw {
		var record sessionAffinityRecord
		if errDecode := json.Unmarshal(item, &record); errDecode != nil || record.AuthID == "" {
			skipped++
			continue
		}
		aliases := compactSessionAliases(mergeSessionAliases(nil, record.Aliases...))
		if len(aliases) == 0 || !now.Before(record.ExpiresAt) {
			skipped++
			continue
		}
		conflict := false
		for _, alias := range aliases {
			if _, exists := c.entries[alias]; exists {
				conflict = true
				break
			}
		}
		if conflict {
			skipped++
			continue
		}
		binding := sessionBinding{sessionID: record.BindingSession, provider: record.BindingProvider, sequence: record.BindingSequence}
		if binding.sessionID == "" || binding.provider == "" || binding.sequence == 0 {
			binding = sessionBinding{}
		} else if binding.sequence > c.bindingSequence {
			c.bindingSequence = binding.sequence
		}
		c.replaceAliasGroupsLocked(record.AuthID, record.ExpiresAt, aliases, binding)
		loaded++
	}
	c.writer = writer
	if skipped > 0 || status == statefile.LoadQuarantined {
		writer.MarkDirty()
	}
	groups := len(c.groups)
	c.mu.Unlock()
	log.Infof("session-affinity: persistence enabled | file=%s loaded=%d kept=%d skipped=%d", path, loaded, groups, skipped)
	return writer
}

// snapshotState runs with c.mu held by the writer.
func (c *SessionCache) snapshotState() ([]byte, error) {
	records := make([]sessionAffinityRecord, 0, len(c.groups))
	if c.evictionOrder != nil {
		for element := c.evictionOrder.Front(); element != nil; element = element.Next() {
			primaryKey, _ := element.Value.(string)
			group, ok := c.groups[primaryKey]
			if !ok {
				continue
			}
			records = append(records, sessionAffinityRecord{
				AuthID:    group.authID,
				ExpiresAt: group.expiresAt,
				Aliases:   append([]string(nil), group.aliases...),

				BindingSession:  group.binding.sessionID,
				BindingProvider: group.binding.provider,
				BindingSequence: group.binding.sequence,
			})
		}
	}
	return statefile.Encode(sessionAffinityStateVersion, records)
}
