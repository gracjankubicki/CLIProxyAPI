package accountmodels

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type Snapshot struct {
	Identity  string    `json:"identity"`
	Provider  string    `json:"provider"`
	AuthIndex string    `json:"auth_index"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
	Error     string    `json:"error,omitempty"`
}

type Status struct {
	Enabled    bool       `json:"enabled"`
	Running    bool       `json:"running"`
	StartedAt  time.Time  `json:"started_at,omitempty"`
	FinishedAt time.Time  `json:"finished_at,omitempty"`
	Accounts   []Snapshot `json:"accounts"`
}

// Manager owns discovery, persistent last-good snapshots and bounded verification.
// Publication is delegated to Service, which fences auth/config changes and retains cooldowns.
type Manager struct {
	auth      *coreauth.Manager
	config    func() *config.Config
	publish   func(*coreauth.Auth, []*registry.ModelInfo) bool
	mu        sync.Mutex
	snapshots map[string]Snapshot
	status    Status
	wake      chan struct{}
	fetch     func(context.Context, *coreauth.Auth) ([]byte, error)
	probe     func(context.Context, *coreauth.Auth, Model) error
	allowed   func(*coreauth.Auth, string) bool
}

func New(auth *coreauth.Manager, cfg func() *config.Config, publish func(*coreauth.Auth, []*registry.ModelInfo) bool, allowed func(*coreauth.Auth, string) bool) *Manager {
	m := &Manager{auth: auth, config: cfg, publish: publish, snapshots: map[string]Snapshot{}, wake: make(chan struct{}, 1)}
	m.fetch = m.fetchCatalog
	m.probe = m.verifyToolLoop
	m.allowed = allowed
	return m
}

func eligible(a *coreauth.Auth) bool {
	return a != nil && !a.Disabled && a.Status != coreauth.StatusDisabled && a.AuthKind() != "apikey" && (a.Provider == "codex" || a.Provider == "claude")
}

func identity(a *coreauth.Auth) string {
	if a == nil {
		return ""
	}
	return kvstore.HashKeyPart(fmt.Sprint(a.Provider, "\x00", a.ID, "\x00", a.Metadata["account_id"], "\x00", a.Metadata["email"]))
}

func (m *Manager) enabled() bool {
	cfg := m.config()
	return cfg != nil && cfg.AccountModelDiscovery.Enabled && !cfg.Home.Enabled
}

func (m *Manager) load(a *coreauth.Auth) (Snapshot, bool) {
	key := identity(a)
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.snapshots[key]; ok {
		return s, true
	}
	var s Snapshot
	_, found, err := kvstore.KVGetJSONRequired(context.Background(), "account-models:v1:"+key, &s)
	if err == nil && found && s.Identity == key && s.Provider == a.Provider {
		m.snapshots[key] = s
		return s, true
	}
	return Snapshot{}, false
}

// Models never performs network I/O. A successful empty snapshot stays authoritative.
func (m *Manager) Models(a *coreauth.Auth) ([]*registry.ModelInfo, bool) {
	if m == nil || !m.enabled() || !eligible(a) {
		return nil, false
	}
	s, ok := m.load(a)
	if !ok {
		m.trigger(true)
		return nil, false
	}
	return infos(s), true
}

func infos(s Snapshot) []*registry.ModelInfo {
	models := make([]*registry.ModelInfo, 0, len(s.Models))
	for _, model := range s.Models {
		if model.Verification.Status == "failed" || model.Verification.Status == "pending" || model.Verification.Status == "deferred" {
			continue
		}
		models = append(models, model.Info(s.Provider))
	}
	return models
}

// Trigger coalesces button clicks and account updates into one asynchronous run.
func (m *Manager) Trigger() Status {
	return m.trigger(false)
}

// A cache miss during an active run queues one follow-up, so an account added
// after that run captured its auth list is discovered without waiting for the timer.
func (m *Manager) trigger(missingAccount bool) Status {
	if m == nil {
		return Status{}
	}
	enabled := m.enabled()
	m.mu.Lock()
	if enabled && (!m.status.Running || missingAccount) {
		if !m.status.Running {
			m.status.Running = true
			m.status.StartedAt = time.Now().UTC()
		}
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
	m.mu.Unlock()
	return m.Status()
}

func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}
	enabled := m.enabled()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.status
	out.Enabled = enabled
	// Copy JSON-shaped metadata before releasing the lock.
	raw, _ := json.Marshal(out)
	_ = json.Unmarshal(raw, &out)
	for i := range out.Accounts {
		for j := range out.Accounts[i].Models {
			out.Accounts[i].Models[j].ClientMetadata = nil
		}
	}
	if out.Accounts == nil {
		out.Accounts = []Snapshot{}
	}
	return out
}

func (m *Manager) Run(ctx context.Context) {
	m.Trigger()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seconds := 10800
			if cfg := m.config(); cfg != nil && cfg.AccountModelDiscovery.RefreshSeconds >= 60 {
				seconds = cfg.AccountModelDiscovery.RefreshSeconds
			}
			if time.Since(last) >= time.Duration(seconds)*time.Second {
				m.Trigger()
			}
		case <-m.wake:
			if m.enabled() {
				m.refresh(ctx)
			}
			last = time.Now()
			m.mu.Lock()
			if len(m.wake) == 0 {
				m.status.Running = false
				m.status.FinishedAt = last.UTC()
			}
			m.mu.Unlock()
		}
	}
}

func (m *Manager) refresh(ctx context.Context) {
	accounts := m.auth.List()
	results := make([]Snapshot, 0, len(accounts))
	// Sequential account requests bound inference concurrency and simplify provider limits.
	for _, a := range accounts {
		if !eligible(a) {
			continue
		}
		if ctx.Err() != nil {
			break
		}
		old, hasOld := m.load(a)
		s := old
		s.Identity = identity(a)
		s.Provider = a.Provider
		s.AuthIndex = a.Index
		if a.Provider == "codex" {
			s.Source = CodexURL
		} else {
			s.Source = ClaudeURL
		}
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		raw, err := m.fetch(requestCtx, a)
		cancel()
		var models []Model
		if err == nil {
			models, err = Parse(a.Provider, raw)
		}
		if err != nil {
			s.Error = safeError(err)
			if !hasOld {
				s.Models = []Model{}
			}
			results = append(results, s)
			continue
		}
		previous := map[string]Model{}
		for _, model := range old.Models {
			previous[model.ID] = model
		}
		cfg := m.config()
		verify := cfg.AccountModelDiscovery.VerifyNewModels == nil || *cfg.AccountModelDiscovery.VerifyNewModels
		for i := range models {
			model := &models[i]
			fingerprint := model.fingerprint()
			if m.allowed != nil && !m.allowed(a, model.ID) {
				model.Excluded = true
				if registry.LookupStaticModelInfoByChannel(model.ID, a.Provider) != nil {
					model.Verification = Verification{Status: "known", Fingerprint: fingerprint}
				}
				continue
			}
			if p, ok := previous[model.ID]; ok && p.Verification.Fingerprint == fingerprint && p.Verification.Status == "passed" {
				model.Verification = p.Verification
			} else if model.Hidden {
				model.Verification = Verification{Status: "hidden", Fingerprint: fingerprint}
			} else if _, ok := previous[model.ID]; !ok && registry.LookupStaticModelInfoByChannel(model.ID, a.Provider) != nil {
				model.Verification = Verification{Status: "known", Fingerprint: fingerprint, Scope: "existing project definition; not an inference test"}
			} else if p, ok := previous[model.ID]; ok && p.Verification.Fingerprint == fingerprint && p.Verification.Status == "known" {
				model.Verification = p.Verification
			} else if verify {
				if probeBlocked(a, model.ID, time.Now()) {
					model.Verification = Verification{Status: "deferred", Fingerprint: fingerprint, Error: "account is cooling down"}
					if old, ok := previous[model.ID]; ok && (old.Verification.Status == "known" || old.Verification.Status == "passed") {
						pending, check := model.Parameters, model.Verification
						*model = old
						model.PendingParameters = &pending
						model.PendingVerification = &check
					}
					continue
				}
				probeCtx, stop := context.WithTimeout(ctx, 60*time.Second)
				err = m.probe(probeCtx, a, *model)
				stop()
				model.Verification = Verification{Status: "passed", Fingerprint: fingerprint, CheckedAt: time.Now().UTC(), Scope: "Responses SSE function call and tool-output continuation"}
				if err != nil {
					model.Verification.Status = "failed"
					model.Verification.Error = safeError(err)
					if previous, ok := previous[model.ID]; ok && (previous.Verification.Status == "passed" || previous.Verification.Status == "known") {
						pendingParameters, pendingCheck := model.Parameters, model.Verification
						*model = previous
						model.PendingParameters = &pendingParameters
						model.PendingVerification = &pendingCheck
					}
				}
			} else {
				model.Verification = Verification{Status: "unchecked", Fingerprint: fingerprint}
			}
		}
		if ctx.Err() != nil {
			break
		}
		latest, ok := m.auth.GetByID(a.ID)
		if !ok || !eligible(latest) || identity(latest) != identity(a) || latest.RegistrationEpoch != a.RegistrationEpoch || latest.CredentialVersion != a.CredentialVersion {
			continue
		}
		s.Models = models
		s.FetchedAt = time.Now().UTC()
		s.Error = ""
		// Apply exclusions/aliases using the latest config and reject obsolete auth publications.
		if m.publish != nil && !m.publish(latest, infos(s)) {
			s.Error = "account changed during publication"
			results = append(results, s)
			continue
		}
		m.mu.Lock()
		m.snapshots[s.Identity] = s
		m.mu.Unlock()
		if kvMode, errSave := kvstore.KVSetJSONRequired(ctx, "account-models:v1:"+s.Identity, s, 0); !kvMode || errSave != nil {
			s.Error = "catalog active; persistent cache unavailable"
		}
		results = append(results, s)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].AuthIndex < results[j].AuthIndex })
	m.mu.Lock()
	m.status.Accounts = results
	m.mu.Unlock()
}

func probeBlocked(a *coreauth.Auth, model string, now time.Time) bool {
	blocked := func(unavailable bool, retry time.Time, quota coreauth.QuotaState) bool {
		return (unavailable && (retry.IsZero() || now.Before(retry))) ||
			(quota.Exceeded && (quota.NextRecoverAt.IsZero() || now.Before(quota.NextRecoverAt)))
	}
	if blocked(a.Unavailable, a.NextRetryAfter, a.Quota) {
		return true
	}
	if state := a.ModelStates[model]; state != nil {
		return state.Status == coreauth.StatusDisabled || blocked(state.Unavailable, state.NextRetryAfter, state.Quota)
	}
	return false
}

func (m *Manager) fetchCatalog(ctx context.Context, a *coreauth.Auth) ([]byte, error) {
	u := CodexURL
	if a.Provider == "claude" {
		u = ClaudeURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("catalog request failed")
	}
	req.Header.Set("Accept", "application/json")
	if a.Provider == "codex" {
		req.Header.Set("Originator", "codex_cli_rs")
		req.Header.Set("User-Agent", "codex_cli_rs/"+ClientVersion)
	} else {
		req.Header.Set("Anthropic-Version", "2023-06-01")
		req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	}
	res, err := m.auth.HttpRequest(ctx, a, req)
	if err != nil {
		return nil, fmt.Errorf("catalog request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("catalog HTTP %d", res.StatusCode)
	}
	const limit = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(raw) > limit {
		return nil, fmt.Errorf("catalog body incomplete or too large")
	}
	return raw, nil
}

// Never include provider bodies, tokens, URLs with query data or raw executor errors.
func safeError(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	for _, known := range []string{"catalog HTTP ", "invalid catalog JSON", "missing model array", "invalid model array", "invalid or duplicate model identifier", "incomplete paginated catalog", "negative model limit", "catalog request failed", "catalog body incomplete or too large", "tool call missing", "tool continuation failed", "probe HTTP ", "probe stream incomplete", "probe stream failed", "probe request failed", "probe executor unavailable", "invalid model parameters"} {
		if strings.HasPrefix(text, known) && len(text) < 100 {
			return text
		}
	}
	return "verification or discovery failed"
}
