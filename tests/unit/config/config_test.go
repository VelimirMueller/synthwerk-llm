package config_test

import (
	"strings"
	"testing"

	"github.com/VelimirMueller/synthwerk-llm/internal/config"
)

func envOf(m map[string]string) func(string) string {
	base := map[string]string{
		"SYNTHWERK_LLM_PROVIDER":      "ollama",
		"SYNTHWERK_LLM_OIDC_ISSUER":   "http://auth.localhost:8080",
		"SYNTHWERK_LLM_OIDC_AUDIENCE": "synthwerk-api",
		"SYNTHWERK_LLM_NATS_URL":      "nats://bus:4222",
	}
	for k, v := range m {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := config.Load(envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "http://ollama:11434/v1" || c.Aliases["default"] != "qwen3.5:4b" {
		t.Fatalf("defaults: base %q aliases %v", c.BaseURL, c.Aliases)
	}
	if c.MaxInputChars != 8000 || c.MaxStreamsPerOwner != 2 || c.ProviderConcurrency != 1 || c.StreamTimeout.Seconds() != 120 {
		t.Fatalf("limits: %+v", c)
	}
	if c.QuotaUserTokens != 50000 || c.QuotaOrgTokens != 500000 || c.QuotaReserveTokens != 1024 {
		t.Fatalf("quota: user %d org %d reserve %d", c.QuotaUserTokens, c.QuotaOrgTokens, c.QuotaReserveTokens)
	}
}

func TestLoadQuotaZeroMeansNoLimit(t *testing.T) {
	c, err := config.Load(envOf(map[string]string{"SYNTHWERK_LLM_QUOTA_USER_TOKENS": "0", "SYNTHWERK_LLM_QUOTA_ORG_TOKENS": "0"}))
	if err != nil || c.QuotaUserTokens != 0 || c.QuotaOrgTokens != 0 {
		t.Fatalf("got user %d org %d, err %v", c.QuotaUserTokens, c.QuotaOrgTokens, err)
	}
}

func TestLoadRefuses(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"empty provider":          {map[string]string{"SYNTHWERK_LLM_PROVIDER": ""}, "SYNTHWERK_LLM_PROVIDER is required"},
		"unknown provider":        {map[string]string{"SYNTHWERK_LLM_PROVIDER": "anthropic"}, "is unknown"},
		"fake in prd":             {map[string]string{"SYNTHWERK_LLM_PROVIDER": "fake", "SYNTHWERK_PLATFORM_ENV": "prd"}, "fake provider is refused in prd"},
		"fake in stg":             {map[string]string{"SYNTHWERK_LLM_PROVIDER": "fake", "SYNTHWERK_PLATFORM_ENV": "stg"}, "fake provider is refused in stg"},
		"openai_compat no url":    {map[string]string{"SYNTHWERK_LLM_PROVIDER": "openai_compat"}, "BASE_URL is required"},
		"no default alias":        {map[string]string{"SYNTHWERK_LLM_MODEL_ALIASES": "small=x"}, `alias "default"`},
		"bad alias":               {map[string]string{"SYNTHWERK_LLM_MODEL_ALIASES": "Default=x"}, "is not alias=model"},
		"no issuer":               {map[string]string{"SYNTHWERK_LLM_OIDC_ISSUER": ""}, "OIDC_ISSUER is required"},
		"no audience":             {map[string]string{"SYNTHWERK_LLM_OIDC_AUDIENCE": " , "}, "OIDC_AUDIENCE is required"},
		"input above contract":    {map[string]string{"SYNTHWERK_LLM_MAX_INPUT_CHARS": "9000"}, "<= 8000"},
		"bad stream timeout":      {map[string]string{"SYNTHWERK_LLM_STREAM_TIMEOUT": "soon"}, "STREAM_TIMEOUT"},
		"zero concurrency":        {map[string]string{"SYNTHWERK_LLM_PROVIDER_CONCURRENCY": "0"}, "PROVIDER_CONCURRENCY"},
		"unknown platform env":    {map[string]string{"SYNTHWERK_PLATFORM_ENV": "qa"}, "SYNTHWERK_PLATFORM_ENV"},
		"no nats url":             {map[string]string{"SYNTHWERK_LLM_NATS_URL": ""}, "NATS_URL is required"},
		"unreadable api key file": {map[string]string{"SYNTHWERK_LLM_API_KEY_FILE": "/does/not/exist"}, "API_KEY_FILE"},
		"negative user quota":     {map[string]string{"SYNTHWERK_LLM_QUOTA_USER_TOKENS": "-1"}, "QUOTA_USER_TOKENS must be an integer"},
		"org quota not a number":  {map[string]string{"SYNTHWERK_LLM_QUOTA_ORG_TOKENS": "lots"}, "QUOTA_ORG_TOKENS must be an integer"},
		"quota above contract":    {map[string]string{"SYNTHWERK_LLM_QUOTA_ORG_TOKENS": "1000000000001"}, "QUOTA_ORG_TOKENS must be an integer"},
		"user quota at reserve":   {map[string]string{"SYNTHWERK_LLM_QUOTA_USER_TOKENS": "1024"}, "QUOTA_USER_TOKENS must be 0 or above"},
		"org quota below reserve": {map[string]string{"SYNTHWERK_LLM_QUOTA_ORG_TOKENS": "2000", "SYNTHWERK_LLM_QUOTA_RESERVE_TOKENS": "4000"}, "QUOTA_ORG_TOKENS must be 0 or above"},
		"zero reserve":            {map[string]string{"SYNTHWERK_LLM_QUOTA_RESERVE_TOKENS": "0"}, "QUOTA_RESERVE_TOKENS"},
		"identity url relative":   {map[string]string{"SYNTHWERK_LLM_IDENTITY_URL": "identity:8080"}, "IDENTITY_URL must be an absolute"},
		"identity url with query": {map[string]string{"SYNTHWERK_LLM_IDENTITY_URL": "http://identity:8080/?x=1"}, "IDENTITY_URL must be an absolute"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(envOf(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadAPIKeyNeverInError(t *testing.T) {
	_, err := config.Load(envOf(map[string]string{"SYNTHWERK_LLM_API_KEY": "sk-canary-123", "SYNTHWERK_LLM_PROVIDER": "nope"}))
	if err == nil || strings.Contains(err.Error(), "sk-canary-123") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadFakeInDev(t *testing.T) {
	c, err := config.Load(envOf(map[string]string{"SYNTHWERK_LLM_PROVIDER": "fake", "SYNTHWERK_PLATFORM_ENV": "dev", "SYNTHWERK_LLM_FAKE_DELAY": "50ms"}))
	if err != nil || c.FakeDelay.Milliseconds() != 50 {
		t.Fatalf("c=%+v err=%v", c, err)
	}
}

func TestLoadIdentityURL(t *testing.T) {
	c, err := config.Load(envOf(map[string]string{"SYNTHWERK_LLM_IDENTITY_URL": "http://identity:8080"}))
	if err != nil || c.IdentityURL != "http://identity:8080" {
		t.Fatalf("c=%+v err=%v", c.IdentityURL, err)
	}
	c, err = config.Load(envOf(nil))
	if err != nil || c.IdentityURL != "" {
		t.Fatalf("unset: %q %v", c.IdentityURL, err)
	}
}
