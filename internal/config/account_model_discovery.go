package config

// AccountModelDiscoveryConfig opts OAuth accounts into live model discovery.
// Verification is a bounded generation/tool-loop probe of newly discovered models.
type AccountModelDiscoveryConfig struct {
	Enabled         bool  `yaml:"enabled" json:"enabled"`
	RefreshSeconds  int   `yaml:"refresh-seconds" json:"refresh-seconds"`
	VerifyNewModels *bool `yaml:"verify-new-models,omitempty" json:"verify-new-models,omitempty"`
}
