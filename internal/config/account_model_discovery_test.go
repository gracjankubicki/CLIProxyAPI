package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountModelDiscoveryV8RoundTrip(t *testing.T) {
	raw := []byte("config-version: 8\nrouting:\n  account-model-discovery:\n    enabled: true\n    refresh-seconds: 600\n    verify-new-models: false\n")
	if err := ValidateV8Config(raw); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AccountModelDiscovery.Enabled || cfg.AccountModelDiscovery.RefreshSeconds != 600 || cfg.AccountModelDiscovery.VerifyNewModels == nil || *cfg.AccountModelDiscovery.VerifyNewModels {
		t.Fatal(cfg.AccountModelDiscovery)
	}
	if err := SaveConfigPreserveComments(p, cfg, false); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(p)
	if err != nil || !cfg.AccountModelDiscovery.Enabled {
		t.Fatal("round trip lost discovery config", err)
	}
}
