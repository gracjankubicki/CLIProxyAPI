package registry

import (
	"github.com/MoeForge/moego/json"
	"reflect"
	"testing"
)

func TestAccountMetadataUsesCommonLimitsWithoutMutatingAccounts(t *testing.T) {
	r := newTestModelRegistry()
	a := &ModelInfo{ID: "discovered-common", AccountDiscovered: true, ContextLength: 500000, MaxCompletionTokens: 128000, Thinking: &ThinkingSupport{Levels: []string{"low", "high", "ultra"}}, CodexClientMetadata: []byte(`{"slug":"discovered-common","context_window":500000,"max_context_window":872000,"default_reasoning_level":"ultra","use_responses_lite":true}`)}
	b := cloneModelInfo(a)
	b.ContextLength = 272000
	b.MaxCompletionTokens = 64000
	b.Thinking.Levels = []string{"low", "high"}
	b.CodexClientMetadata = []byte(`{"slug":"discovered-common","context_window":272000,"max_context_window":400000,"default_reasoning_level":"low"}`)
	r.RegisterClient("account-b", "codex", []*ModelInfo{b})
	r.RegisterClient("account-a", "codex", []*ModelInfo{a})
	info := r.GetModelInfo(a.ID, "codex")
	if info.ContextLength != 272000 || info.MaxCompletionTokens != 64000 || !reflect.DeepEqual(info.Thinking.Levels, []string{"low", "high"}) {
		t.Fatalf("common entry %+v", info)
	}
	var meta map[string]any
	_ = json.Unmarshal(info.CodexClientMetadata, &meta)
	if meta["max_context_window"] != float64(400000) || meta["default_reasoning_level"] != "low" || meta["use_responses_lite"] != false {
		t.Fatal(meta)
	}
	if own := r.GetModelsForClient("account-a"); own[0].ContextLength != 500000 || len(own[0].Thinking.Levels) != 3 {
		t.Fatal("account parameters overwritten")
	}
	for _, list := range [][]*ModelInfo{r.GetAvailableModelInfos(), r.GetAvailableModelsByProvider("codex")} {
		if len(list) != 1 || list[0].ContextLength != 272000 || list[0].MaxCompletionTokens != 64000 {
			t.Fatal("public list did not use common limits")
		}
	}
	public := r.GetAvailableModels("openai")
	if len(public) != 1 || public[0]["context_length"] != 272000 || public[0]["max_completion_tokens"] != 64000 {
		t.Fatal("serialized catalog did not use common limits", public)
	}
	info.CodexClientMetadata[0] = 'x'
	if r.GetModelInfo(a.ID, "codex").CodexClientMetadata[0] == 'x' {
		t.Fatal("returned metadata aliases registry storage")
	}
}
