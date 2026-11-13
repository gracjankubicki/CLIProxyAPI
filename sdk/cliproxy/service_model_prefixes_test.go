package cliproxy

import "testing"

func TestApplyModelPrefixesKeepsPrefixRequiredModelsAccountSpecific(t *testing.T) {
	models := []*ModelInfo{
		{ID: "shared-model"},
		{ID: "account-specific-model", RequireModelPrefix: true},
	}

	out := applyModelPrefixes(models, "account-1", false)
	got := make(map[string]struct{}, len(out))
	for _, model := range out {
		if model != nil {
			got[model.ID] = struct{}{}
		}
	}

	for _, want := range []string{"shared-model", "account-1/shared-model", "account-1/account-specific-model"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("applyModelPrefixes() IDs = %v, want %q", got, want)
		}
	}
	if _, ok := got["account-specific-model"]; ok {
		t.Fatalf("applyModelPrefixes() exposed prefix-required model without an account prefix: %v", got)
	}
}

func TestApplyModelPrefixesHidesPrefixRequiredModelsWithoutAccountPrefix(t *testing.T) {
	models := []*ModelInfo{
		{ID: "shared-model"},
		{ID: "account-specific-model", RequireModelPrefix: true},
	}

	out := applyModelPrefixes(models, "", false)
	if len(out) != 1 || out[0] == nil || out[0].ID != "shared-model" {
		t.Fatalf("applyModelPrefixes() without account prefix = %#v, want only shared-model", out)
	}
}
