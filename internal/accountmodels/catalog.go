// Package accountmodels discovers the models and parameters granted to each OAuth account.
package accountmodels

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const ClientVersion = "0.169.0"
const CodexURL = "https://chatgpt.com/backend-api/codex/models?client_version=" + ClientVersion
const ClaudeURL = "https://api.anthropic.com/v1/models?limit=1000"

var modelID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,199}$`)

// Parameters preserves distinct official limits. Zero/empty means not supplied,
// never a guessed context window or reasoning default.
type Parameters struct {
	ContextWindow    int                        `json:"context_window,omitempty"`
	MaxContextWindow int                        `json:"max_context_window,omitempty"`
	MaxOutputTokens  int                        `json:"max_output_tokens,omitempty"`
	ReasoningLevels  []string                   `json:"reasoning_levels,omitempty"`
	DefaultReasoning string                     `json:"default_reasoning,omitempty"`
	InputModalities  []string                   `json:"input_modalities,omitempty"`
	OutputModalities []string                   `json:"output_modalities,omitempty"`
	Capabilities     map[string]json.RawMessage `json:"capabilities,omitempty"`
}

type Verification struct {
	Status      string    `json:"status"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	CheckedAt   time.Time `json:"checked_at,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type Model struct {
	ID                  string          `json:"id"`
	DisplayName         string          `json:"display_name"`
	Parameters          Parameters      `json:"parameters"`
	ClientMetadata      json.RawMessage `json:"client_metadata,omitempty"`
	Verification        Verification    `json:"verification"`
	Hidden              bool            `json:"hidden,omitempty"`
	Excluded            bool            `json:"excluded,omitempty"`
	PendingParameters   *Parameters     `json:"pending_parameters,omitempty"`
	PendingVerification *Verification   `json:"pending_verification,omitempty"`
}

func (m Model) fingerprint() string {
	data, _ := json.Marshal(struct {
		ID         string
		Parameters Parameters
		Metadata   json.RawMessage
		Version    string
	}{m.ID, m.Parameters, m.ClientMetadata, "responses-tool-loop-v1"})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Parse rejects malformed/partial catalogs; an explicitly empty array is a valid removal.
func Parse(provider string, raw []byte) ([]Model, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return nil, fmt.Errorf("invalid catalog JSON")
	}
	key := "models"
	if provider == "claude" {
		key = "data"
	}
	value, ok := root[key]
	if !ok || string(value) == "null" {
		return nil, fmt.Errorf("missing model array")
	}
	var records []map[string]json.RawMessage
	if json.Unmarshal(value, &records) != nil {
		return nil, fmt.Errorf("invalid model array")
	}
	var more bool
	_ = json.Unmarshal(root["has_more"], &more)
	if more {
		return nil, fmt.Errorf("incomplete paginated catalog")
	}
	seen := map[string]bool{}
	models := make([]Model, 0, len(records))
	for _, record := range records {
		var m Model
		idField := "slug"
		if provider == "claude" {
			idField = "id"
		}
		_ = json.Unmarshal(record[idField], &m.ID)
		if !modelID.MatchString(m.ID) || seen[m.ID] {
			return nil, fmt.Errorf("invalid or duplicate model identifier")
		}
		seen[m.ID] = true
		_ = json.Unmarshal(record["display_name"], &m.DisplayName)
		if m.DisplayName == "" {
			m.DisplayName = m.ID
		}
		p := &m.Parameters
		for key, target := range map[string]any{"context_window": &p.ContextWindow, "max_context_window": &p.MaxContextWindow, "max_input_tokens": &p.ContextWindow, "max_tokens": &p.MaxOutputTokens, "max_output_tokens": &p.MaxOutputTokens, "default_reasoning_level": &p.DefaultReasoning, "input_modalities": &p.InputModalities, "output_modalities": &p.OutputModalities} {
			if value, ok := record[key]; ok && string(value) != "null" {
				if err := json.Unmarshal(value, target); err != nil {
					return nil, fmt.Errorf("invalid model parameters")
				}
			}
		}
		if provider == "codex" {
			p.Capabilities = map[string]json.RawMessage{}
			for _, key := range []string{"supports_search_tool", "web_search_tool_type", "apply_patch_tool_type", "supports_parallel_tool_calls", "supports_reasoning_summaries", "supports_reasoning_summary_parameter", "support_verbosity", "supports_image_detail_original", "tool_mode", "supported_in_api"} {
				if value, ok := record[key]; ok {
					p.Capabilities[key] = value
				}
			}
			_ = json.Unmarshal(record["context_window"], &p.ContextWindow)
			_ = json.Unmarshal(record["max_context_window"], &p.MaxContextWindow)
			_ = json.Unmarshal(record["max_tokens"], &p.MaxOutputTokens)
			_ = json.Unmarshal(record["default_reasoning_level"], &p.DefaultReasoning)
			_ = json.Unmarshal(record["input_modalities"], &p.InputModalities)
			var levels []struct {
				Effort string `json:"effort"`
			}
			_ = json.Unmarshal(record["supported_reasoning_levels"], &levels)
			for _, level := range levels {
				if level.Effort != "" {
					p.ReasoningLevels = append(p.ReasoningLevels, level.Effort)
				}
			}
			var visibility string
			_ = json.Unmarshal(record["visibility"], &visibility)
			m.Hidden = visibility == "hide"
			// Full Responses retains explicit tool schemas for gateway clients.
			record["use_responses_lite"] = json.RawMessage("false")
			m.ClientMetadata, _ = json.Marshal(record)
		} else if provider == "claude" {
			_ = json.Unmarshal(record["max_input_tokens"], &p.ContextWindow)
			p.MaxContextWindow = p.ContextWindow // Anthropic exposes one input limit.
			_ = json.Unmarshal(record["max_tokens"], &p.MaxOutputTokens)
			if p.MaxOutputTokens == 0 {
				_ = json.Unmarshal(record["max_output_tokens"], &p.MaxOutputTokens)
			}
			_ = json.Unmarshal(record["capabilities"], &p.Capabilities)
			var effort map[string]struct {
				Supported bool `json:"supported"`
			}
			_ = json.Unmarshal(p.Capabilities["effort"], &effort)
			for _, level := range []string{"low", "medium", "high", "xhigh", "max", "ultra"} {
				if effort[level].Supported {
					p.ReasoningLevels = append(p.ReasoningLevels, level)
				}
			}
		} else {
			return nil, fmt.Errorf("unsupported provider")
		}
		if p.ContextWindow < 0 || p.MaxContextWindow < 0 || p.MaxOutputTokens < 0 {
			return nil, fmt.Errorf("negative model limit")
		}
		m.Verification.Status = "pending"
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func (m Model) Info(provider string) *registry.ModelInfo {
	info := registry.LookupStaticModelInfoByChannel(m.ID, provider)
	if info == nil {
		info = &registry.ModelInfo{ID: m.ID, Object: "model", Type: provider, OwnedBy: provider}
	}
	info.DisplayName = m.DisplayName
	info.AccountDiscovered = true
	if provider == "codex" {
		info.OwnedBy = "openai"
	}
	if provider == "claude" {
		info.OwnedBy = "anthropic"
	}
	p := m.Parameters
	if p.ContextWindow > 0 {
		info.ContextLength = p.ContextWindow
	}
	if p.MaxOutputTokens > 0 {
		info.MaxCompletionTokens = p.MaxOutputTokens
	}
	if len(p.ReasoningLevels) > 0 {
		if info.Thinking == nil {
			info.Thinking = &registry.ThinkingSupport{}
		}
		info.Thinking.Levels = append([]string(nil), p.ReasoningLevels...)
		info.ExplicitThinking = true
	}
	if len(p.InputModalities) > 0 {
		info.SupportedInputModalities = append([]string(nil), p.InputModalities...)
		info.ExplicitInputModalities = true
	}
	info.CodexClientMetadata = append([]byte(nil), m.ClientMetadata...)
	if provider == "claude" {
		var image struct {
			Supported bool `json:"supported"`
		}
		if json.Unmarshal(p.Capabilities["image_input"], &image) == nil {
			info.SupportedInputModalities = []string{"text"}
			if image.Supported {
				info.SupportedInputModalities = append(info.SupportedInputModalities, "image")
			}
			info.ExplicitInputModalities = true
		}
		var tools struct {
			WebSearch struct {
				Supported bool `json:"supported"`
			} `json:"web_search"`
		}
		if json.Unmarshal(p.Capabilities["server_tools"], &tools) == nil {
			v := tools.WebSearch.Supported
			info.NativeCapabilities = &registry.NativeCapabilities{WebSearch: &v}
			info.SupportsWebSearch = v
		}
	} else if len(m.ClientMetadata) > 0 {
		var metadata struct {
			WebSearch string `json:"web_search_tool_type"`
		}
		if json.Unmarshal(m.ClientMetadata, &metadata) == nil && metadata.WebSearch != "" && metadata.WebSearch != "none" {
			v := true
			info.NativeCapabilities = &registry.NativeCapabilities{WebSearch: &v}
			info.SupportsWebSearch = true
		}
	}
	if strings.HasPrefix(m.ID, "gpt-image-") {
		info.Type = registry.OpenAIImageModelType
	}
	return info
}
