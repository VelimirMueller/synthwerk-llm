// Package registry builds the provider that SYNTHWERK_LLM_PROVIDER names, and the
// fallback chain over it and SYNTHWERK_LLM_FALLBACKS (#56). A new adapter
// (anthropic, E4) is a new package plus one case in adapter.
package registry

import (
	"fmt"
	"log/slog"

	"github.com/VelimirMueller/synthwerk-llm/internal/config"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/chain"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/openaicompat"
)

// New returns the primary provider of the config, without fallbacks. The eval
// (#72) scores it alone.
func New(cfg config.Config) (provider.Provider, error) {
	return adapter(cfg.Provider, cfg.BaseURL, cfg.APIKey, cfg)
}

// Chain returns the gateway's provider: the primary, then the fallbacks in order.
// Without fallbacks the chain has one link and calls the primary only.
func Chain(cfg config.Config, logger *slog.Logger) (*chain.Chain, error) {
	primary, err := New(cfg)
	if err != nil {
		return nil, err
	}
	links := []chain.Link{{Name: cfg.Provider, Provider: primary, Region: chain.Region(cfg.Region), Models: cfg.Aliases}}
	for _, f := range cfg.Fallbacks {
		p, err := adapter(f.Provider, f.BaseURL, f.APIKey, cfg)
		if err != nil {
			return nil, fmt.Errorf("registry: fallback %q: %w", f.Name, err)
		}
		links = append(links, chain.Link{Name: f.Name, Provider: p, Region: chain.Region(f.Region), Models: f.Aliases})
	}
	return chain.New(chain.Config{
		Links:             links,
		FirstTokenTimeout: cfg.FirstTokenTimeout,
		BreakerFailures:   cfg.BreakerFailures,
		BreakerOpen:       cfg.BreakerOpen,
		AllowNonEU:        cfg.AllowNonEU,
		Log:               logger,
	})
}

func adapter(kind, baseURL, apiKey string, cfg config.Config) (provider.Provider, error) {
	switch kind {
	case config.ProviderFake:
		return fake.New(cfg.FakeDelay), nil
	case config.ProviderOllama, config.ProviderOpenAICompat:
		return openaicompat.New(openaicompat.Config{Name: kind, BaseURL: baseURL, APIKey: apiKey})
	default:
		return nil, fmt.Errorf("registry: provider %q is unknown", kind)
	}
}
