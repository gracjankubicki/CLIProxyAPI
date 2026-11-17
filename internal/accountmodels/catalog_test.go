package accountmodels

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
)

const codexFixture = `{"models":[{"slug":"gpt-discovery-probe","display_name":"Discovery model","context_window":500000,"max_context_window":872000,"max_tokens":128000,"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"ultra"}],"input_modalities":["text","image"],"use_responses_lite":true,"visibility":"list","web_search_tool_type":"text_and_image"}]}`

func TestParsePreservesOfficialDistinctLimits(t *testing.T) {
	models, err := Parse("codex", []byte(codexFixture))
	if err != nil {
		t.Fatal(err)
	}
	m := models[0]
	if m.Parameters.ContextWindow != 500000 || m.Parameters.MaxContextWindow != 872000 || m.Parameters.MaxOutputTokens != 128000 || m.Parameters.DefaultReasoning != "low" {
		t.Fatalf("parameters: %+v", m.Parameters)
	}
	if !reflect.DeepEqual(m.Parameters.ReasoningLevels, []string{"low", "high", "ultra"}) {
		t.Fatal(m.Parameters.ReasoningLevels)
	}
	var metadata map[string]any
	_ = json.Unmarshal(m.ClientMetadata, &metadata)
	if metadata["use_responses_lite"] != false {
		t.Fatal("Lite was not disabled")
	}
	info := m.Info("codex")
	if info.ContextLength != 500000 || info.MaxContextLength != 0 || !info.AccountDiscovered || len(info.CodexClientMetadata) == 0 {
		t.Fatalf("info: %+v", info)
	}
	for i := 0; i < 30; i++ {
		again, _ := Parse("codex", []byte(codexFixture))
		if again[0].fingerprint() != m.fingerprint() {
			t.Fatal("unstable fingerprint")
		}
	}
}

func TestClaudeParametersDoNotInventReasoningDefault(t *testing.T) {
	raw := `{"data":[{"id":"claude-discovery-probe","display_name":"Claude probe","max_input_tokens":1000000,"max_tokens":128000,"capabilities":{"effort":{"low":{"supported":true},"high":{"supported":true}},"image_input":{"supported":true},"server_tools":{"web_search":{"supported":true}}}}],"has_more":false}`
	models, err := Parse("claude", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	p := models[0].Parameters
	if p.ContextWindow != 1000000 || p.MaxContextWindow != 1000000 || p.DefaultReasoning != "" || p.MaxOutputTokens != 128000 {
		t.Fatalf("parameters: %+v", p)
	}
	info := models[0].Info("claude")
	if !info.SupportsWebSearch || !reflect.DeepEqual(info.SupportedInputModalities, []string{"text", "image"}) {
		t.Fatalf("capabilities: %+v", info)
	}
}

func TestParseRejectsPartialOrInvalidCatalog(t *testing.T) {
	for _, raw := range []string{`{}`, `{"models":null}`, `{"models":{}}`, `{"models":[{"slug":"bad id"}]}`, `{"models":[{"slug":"m"},{"slug":"m"}]}`, `{"models":[{"slug":"m","context_window":-1}]}`, `{"models":[{"slug":"m","context_window":"large"}]}`, `{"models":[{"slug":"m","input_modalities":true}]}`, `{"models":[],"has_more":true}`} {
		if _, err := Parse("codex", []byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	models, err := Parse("codex", []byte(`{"models":[]}`))
	if err != nil || len(models) != 0 {
		t.Fatal("explicit empty catalog rejected")
	}
}

func TestFingerprintIncludesParameters(t *testing.T) {
	a, _ := Parse("codex", []byte(codexFixture))
	b, _ := Parse("codex", []byte(strings.Replace(codexFixture, "500000", "600000", 1)))
	if a[0].fingerprint() == b[0].fingerprint() {
		t.Fatal("context update did not invalidate verification")
	}
}
