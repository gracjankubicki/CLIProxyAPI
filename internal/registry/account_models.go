package registry

import (
	"sort"

	"github.com/MoeForge/moego/json"
)

// accountModelInfoLocked constructs a conservative common entry without modifying
// each account's full metadata. The caller owns r.mutex's read/write lock.
func (r *ModelRegistry) accountModelInfoLocked(id, provider string, fallback *ModelInfo) *ModelInfo {
	out := cloneModelInfo(fallback)
	if out == nil {
		return nil
	}
	ids := make([]string, 0)
	for client, infos := range r.clientModelInfos {
		if provider != "" && r.clientProviders[client] != provider {
			continue
		}
		if info := infos[id]; info != nil {
			ids = append(ids, client)
		}
	}
	sort.Strings(ids)
	discovered := false
	for _, client := range ids {
		discovered = discovered || r.clientModelInfos[client][id].AccountDiscovered
	}
	if !discovered {
		return out
	}
	var template map[string]any
	var templateMax int
	var commonInputs []string
	hasInputs := false
	for _, client := range ids {
		info := r.clientModelInfos[client][id]
		out.ContextLength = minPositive(out.ContextLength, info.ContextLength)
		out.MaxCompletionTokens = minPositive(out.MaxCompletionTokens, info.MaxCompletionTokens)
		out.MaxContextLength = minPositive(out.MaxContextLength, info.MaxContextLength)
		out.ExplicitThinking = out.ExplicitThinking || info.ExplicitThinking
		if out.Thinking == nil && info.Thinking != nil {
			out.Thinking = cloneModelInfo(info).Thinking
		}
		if info.Thinking != nil && out.Thinking != nil {
			out.Thinking.Levels = intersectAccountStrings(out.Thinking.Levels, info.Thinking.Levels)
		}
		if info.ExplicitInputModalities {
			if !hasInputs {
				commonInputs = append([]string(nil), info.SupportedInputModalities...)
				hasInputs = true
			} else {
				commonInputs = intersectAccountStrings(commonInputs, info.SupportedInputModalities)
			}
		}
		var raw map[string]any
		if len(info.CodexClientMetadata) == 0 || json.Unmarshal(info.CodexClientMetadata, &raw) != nil {
			continue
		}
		if template == nil {
			template = raw
		}
		if n, ok := raw["max_context_window"].(float64); ok {
			templateMax = minPositive(templateMax, int(n))
		}
		for _, key := range []string{"supports_search_tool", "supports_parallel_tool_calls", "supports_image_detail_original"} {
			if value, ok := raw[key].(bool); ok && !value {
				template[key] = false
			}
		}
	}
	if hasInputs {
		out.ExplicitInputModalities = true
		out.SupportedInputModalities = commonInputs
	}
	if template != nil {
		if out.ContextLength > 0 {
			template["context_window"] = out.ContextLength
		}
		if templateMax > 0 {
			template["max_context_window"] = templateMax
		}
		if out.MaxCompletionTokens > 0 {
			template["max_tokens"] = out.MaxCompletionTokens
		}
		if out.Thinking != nil {
			levels := make([]map[string]string, 0, len(out.Thinking.Levels))
			for _, effort := range out.Thinking.Levels {
				levels = append(levels, map[string]string{"effort": effort, "description": effort})
			}
			template["supported_reasoning_levels"] = levels
			defaultLevel, _ := template["default_reasoning_level"].(string)
			found := false
			for _, level := range out.Thinking.Levels {
				found = found || level == defaultLevel
			}
			if !found && len(out.Thinking.Levels) > 0 {
				template["default_reasoning_level"] = out.Thinking.Levels[0]
			}
		}
		if out.ExplicitInputModalities {
			template["input_modalities"] = out.SupportedInputModalities
		}
		template["use_responses_lite"] = false
		out.CodexClientMetadata, _ = json.Marshal(template)
	}
	return out
}

func minPositive(a, b int) int {
	if a <= 0 {
		return b
	}
	if b > 0 && b < a {
		return b
	}
	return a
}

func intersectAccountStrings(a, b []string) []string {
	set := map[string]bool{}
	for _, s := range b {
		set[s] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if set[s] {
			out = append(out, s)
		}
	}
	return out
}
