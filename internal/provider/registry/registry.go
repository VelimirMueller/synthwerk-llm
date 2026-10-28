// Package registry builds the provider that SYNTHWERK_LLM_PROVIDER names. A new
// adapter (anthropic, E4) is a new package plus one case here.
package registry

import (
	"fmt"

	"github.com/VelimirMueller/synthwerk-llm/internal/config"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/openaicompat"
)

// New returns the provider of the config.
func New(cfg config.Config) (provider.Provider, error) {
	switch cfg.Provider {
	case config.ProviderFake:
		return fake.New(cfg.FakeDelay), nil
	case config.ProviderOllama, config.ProviderOpenAICompat:
		return openaicompat.New(openaicompat.Config{Name: cfg.Provider, BaseURL: cfg.BaseURL, APIKey: cfg.APIKey})
	default:
		return nil, fmt.Errorf("registry: provider %q is unknown", cfg.Provider)
	}
}
