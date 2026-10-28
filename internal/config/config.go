// Package config reads the gateway settings from SYNTHWERK_LLM_* variables (kit/env
// names). Load fails on anything unclear: there is no silent fallback to a hosted
// provider.
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/env"
)

// Service is the short service name (env prefix SYNTHWERK_LLM_).
const Service = "llm"

// Provider names SYNTHWERK_LLM_PROVIDER accepts.
const (
	ProviderOllama       = "ollama"
	ProviderOpenAICompat = "openai_compat"
	ProviderFake         = "fake"
)

// Providers lists the accepted provider names.
var Providers = []string{ProviderOllama, ProviderOpenAICompat, ProviderFake}

// DefaultAlias is the model alias a conversation gets without a choice. #72 runs
// its eval against it, so the name is stable.
const DefaultAlias = "default"

// DefaultModelAliases is the alias map without SYNTHWERK_LLM_MODEL_ALIASES. The eval
// (#72) reads it too: a change here changes the model the eval scores.
const DefaultModelAliases = DefaultAlias + "=qwen3.5:4b"

// DefaultSystemPrompt is the built-in system prompt.
const DefaultSystemPrompt = "You are the Synthwerk assistant. Answer briefly and in plain text."

var aliasRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config of the gateway.
type Config struct {
	// Env is SYNTHWERK_PLATFORM_ENV.
	Env string
	// Provider is the adapter name.
	Provider string
	// BaseURL of an OpenAI-compatible endpoint (ollama, openai_compat).
	BaseURL string
	// APIKey for openai_compat. Secret, never logged.
	APIKey string
	// FakeDelay is the delay per piece of the fake provider (local stack, stack test).
	FakeDelay time.Duration
	// Aliases maps a model alias to the provider model name. Always has DefaultAlias.
	Aliases map[string]string
	// SystemPrompt goes first in every model call.
	SystemPrompt string
	// MaxInputChars is the max length of one user message (413 above).
	MaxInputChars int
	// MaxStreamsPerOwner is the max number of open streams of one user (429 above).
	MaxStreamsPerOwner int
	// StreamTimeout bounds one answer.
	StreamTimeout time.Duration
	// ProviderConcurrency is the max number of model calls at once.
	ProviderConcurrency int
	// QueueTimeout is the max wait for a model slot (503 after).
	QueueTimeout time.Duration
	// MaxContextChars bounds the history sent to the model (newest turns win).
	MaxContextChars int
	// OIDCIssuer is the token issuer (the iss claim).
	OIDCIssuer string
	// OIDCAudiences are the accepted aud values.
	OIDCAudiences []string
	// JWKSURL overrides OIDC discovery. Empty: discovery on the issuer.
	JWKSURL string
	// NATSURL of the bus (secret: may hold credentials).
	NATSURL string
}

// Load reads the config. getenv is os.Getenv in production.
func Load(getenv func(string) string) (Config, error) {
	get := func(name, def string) string {
		if v := strings.TrimSpace(getenv(env.Key(Service, name))); v != "" {
			return v
		}
		return def
	}
	var errs []error
	c := Config{Env: getenv("SYNTHWERK_PLATFORM_ENV")}
	if c.Env == "" {
		c.Env = "local"
	}
	if !slices.Contains(env.Environments, c.Env) {
		errs = append(errs, fmt.Errorf("SYNTHWERK_PLATFORM_ENV=%q is not one of %s", c.Env, strings.Join(env.Environments, ", ")))
	}

	c.Provider = get("PROVIDER", "")
	switch c.Provider {
	case "":
		errs = append(errs, fmt.Errorf("%s is required (%s)", env.Key(Service, "PROVIDER"), strings.Join(Providers, ", ")))
	case ProviderOllama:
		c.BaseURL = get("BASE_URL", "http://ollama:11434/v1")
	case ProviderOpenAICompat:
		c.BaseURL = get("BASE_URL", "")
		if c.BaseURL == "" {
			errs = append(errs, fmt.Errorf("%s is required for %s", env.Key(Service, "BASE_URL"), ProviderOpenAICompat))
		}
	case ProviderFake:
		if c.Env == "stg" || c.Env == "prd" {
			errs = append(errs, fmt.Errorf("the fake provider is refused in %s", c.Env))
		}
	default:
		errs = append(errs, fmt.Errorf("%s=%q is unknown (%s)", env.Key(Service, "PROVIDER"), c.Provider, strings.Join(Providers, ", ")))
	}

	key, err := secret(getenv, "API_KEY")
	if err != nil {
		errs = append(errs, err)
	}
	c.APIKey = key

	c.Aliases, err = ParseAliases(get("MODEL_ALIASES", DefaultModelAliases))
	if err != nil {
		errs = append(errs, err)
	}
	c.SystemPrompt = get("SYSTEM_PROMPT", DefaultSystemPrompt)

	ints := []struct {
		name     string
		def, min int
		dst      *int
	}{
		{"MAX_INPUT_CHARS", 8000, 1, &c.MaxInputChars},
		{"MAX_STREAMS_PER_OWNER", 2, 1, &c.MaxStreamsPerOwner},
		{"PROVIDER_CONCURRENCY", 1, 1, &c.ProviderConcurrency},
		{"MAX_CONTEXT_CHARS", 24000, 100, &c.MaxContextChars},
	}
	for _, i := range ints {
		v, err := strconv.Atoi(get(i.name, strconv.Itoa(i.def)))
		if err != nil || v < i.min {
			errs = append(errs, fmt.Errorf("%s must be an integer >= %d", env.Key(Service, i.name), i.min))
		}
		*i.dst = v
	}
	if c.MaxInputChars > 8000 {
		errs = append(errs, fmt.Errorf("%s must be <= 8000 (contract limit)", env.Key(Service, "MAX_INPUT_CHARS")))
	}
	durs := []struct {
		name string
		def  string
		dst  *time.Duration
	}{
		{"STREAM_TIMEOUT", "120s", &c.StreamTimeout},
		{"QUEUE_TIMEOUT", "10s", &c.QueueTimeout},
		{"FAKE_DELAY", "0s", &c.FakeDelay},
	}
	for _, d := range durs {
		v, err := time.ParseDuration(get(d.name, d.def))
		if err != nil || v < 0 {
			errs = append(errs, fmt.Errorf("%s must be a duration like 30s", env.Key(Service, d.name)))
		}
		*d.dst = v
	}

	c.OIDCIssuer = get("OIDC_ISSUER", "")
	if c.OIDCIssuer == "" {
		errs = append(errs, fmt.Errorf("%s is required", env.Key(Service, "OIDC_ISSUER")))
	}
	for a := range strings.SplitSeq(get("OIDC_AUDIENCE", ""), ",") {
		if a = strings.TrimSpace(a); a != "" {
			c.OIDCAudiences = append(c.OIDCAudiences, a)
		}
	}
	if len(c.OIDCAudiences) == 0 {
		errs = append(errs, fmt.Errorf("%s is required", env.Key(Service, "OIDC_AUDIENCE")))
	}
	c.JWKSURL = get("JWKS_URL", "")
	c.NATSURL = get("NATS_URL", "")
	if c.NATSURL == "" {
		errs = append(errs, fmt.Errorf("%s is required", env.Key(Service, "NATS_URL")))
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return c, nil
}

// secret reads NAME from the env or from the file in NAME_FILE.
func secret(getenv func(string) string, name string) (string, error) {
	if v := getenv(env.Key(Service, name)); v != "" {
		return v, nil
	}
	path := getenv(env.Key(Service, name+"_FILE"))
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the path comes from the operator's env
	if err != nil {
		return "", fmt.Errorf("%s: file cannot be read", env.Key(Service, name+"_FILE"))
	}
	return strings.TrimSpace(string(b)), nil
}

// ParseAliases parses "default=qwen3.5:4b,small=qwen3.5:0.8b". The alias "default"
// must exist.
func ParseAliases(s string) (map[string]string, error) {
	out := map[string]string{}
	for pair := range strings.SplitSeq(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		alias, model, ok := strings.Cut(pair, "=")
		alias, model = strings.TrimSpace(alias), strings.TrimSpace(model)
		if !ok || !aliasRe.MatchString(alias) || model == "" {
			return nil, fmt.Errorf("%s: %q is not alias=model", env.Key(Service, "MODEL_ALIASES"), pair)
		}
		out[alias] = model
	}
	if _, ok := out[DefaultAlias]; !ok {
		return nil, fmt.Errorf("%s must define the alias %q", env.Key(Service, "MODEL_ALIASES"), DefaultAlias)
	}
	return out, nil
}

// ValidAlias reports whether s has the alias shape of the contract.
func ValidAlias(s string) bool { return aliasRe.MatchString(s) }
