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
		"widget issuer relative":  {map[string]string{"SYNTHWERK_LLM_WIDGET_ISSUER": "identity"}, "WIDGET_ISSUER must be an absolute"},
		"widget issuer = oidc":    {map[string]string{"SYNTHWERK_LLM_WIDGET_ISSUER": "http://auth.localhost:8080"}, "WIDGET_ISSUER must differ"},
		"widget jwks relative": {map[string]string{
			"SYNTHWERK_LLM_WIDGET_ISSUER": "http://localhost:8080/identity", "SYNTHWERK_LLM_WIDGET_JWKS_URL": "/keys",
		}, "WIDGET_JWKS_URL must be an absolute"},
		"widget audience empty": {map[string]string{
			"SYNTHWERK_LLM_WIDGET_ISSUER": "http://localhost:8080/identity", "SYNTHWERK_LLM_WIDGET_AUDIENCE": " , ",
		}, "WIDGET_AUDIENCE must not be empty"},
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

// Load - widget issuer (#30): off by default; key set and audience default from the issuer.
func TestLoadWidgetIssuer(t *testing.T) {
	c, err := config.Load(envOf(nil))
	if err != nil || c.WidgetIssuer != "" || c.WidgetJWKSURL != "" || c.WidgetAudiences != nil {
		t.Fatalf("default: %q %q %v, err %v", c.WidgetIssuer, c.WidgetJWKSURL, c.WidgetAudiences, err)
	}
	c, err = config.Load(envOf(map[string]string{"SYNTHWERK_LLM_WIDGET_ISSUER": "http://localhost:8080/identity/"}))
	if err != nil || c.WidgetJWKSURL != "http://localhost:8080/identity/.well-known/jwks.json" ||
		len(c.WidgetAudiences) != 1 || c.WidgetAudiences[0] != config.DefaultWidgetAudience {
		t.Fatalf("derived: %q %v, err %v", c.WidgetJWKSURL, c.WidgetAudiences, err)
	}
	c, err = config.Load(envOf(map[string]string{
		"SYNTHWERK_LLM_WIDGET_ISSUER":   "http://localhost:8080/identity",
		"SYNTHWERK_LLM_WIDGET_JWKS_URL": "http://identity:8080/.well-known/jwks.json",
		"SYNTHWERK_LLM_WIDGET_AUDIENCE": "a, b",
	}))
	if err != nil || c.WidgetJWKSURL != "http://identity:8080/.well-known/jwks.json" || strings.Join(c.WidgetAudiences, ",") != "a,b" {
		t.Fatalf("set: %q %v, err %v", c.WidgetJWKSURL, c.WidgetAudiences, err)
	}
}

// Load - CORS origins of the widget chat routes (infra#21): empty by default,
// canonical form, an invalid item refuses the start.
func TestLoadCORSOrigins(t *testing.T) {
	c, err := config.Load(envOf(nil))
	if err != nil || len(c.CORSOrigins) != 0 {
		t.Fatalf("default: %v %v", c.CORSOrigins, err)
	}
	c, err = config.Load(envOf(map[string]string{"SYNTHWERK_LLM_CORS_ORIGINS": "http://localhost:3000, HTTPS://Studio.Example:443"}))
	if err != nil || len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != "http://localhost:3000" || c.CORSOrigins[1] != "https://studio.example" {
		t.Fatalf("list: %v %v", c.CORSOrigins, err)
	}
	for _, bad := range []string{"*", "https://studio.example/", "http://studio.example", "null"} {
		if _, err := config.Load(envOf(map[string]string{"SYNTHWERK_LLM_CORS_ORIGINS": bad})); err == nil || !strings.Contains(err.Error(), "SYNTHWERK_LLM_CORS_ORIGINS") {
			t.Fatalf("%q accepted: %v", bad, err)
		}
	}
}

// fallbackEnv is one EU fallback "backup" on openai_compat for the alias default.
func fallbackEnv(extra map[string]string) map[string]string {
	m := map[string]string{
		"SYNTHWERK_LLM_MODEL_ALIASES":                 "default=qwen3.5:4b,small=qwen3.5:0.8b",
		"SYNTHWERK_LLM_FALLBACKS":                     "backup",
		"SYNTHWERK_LLM_FALLBACK_BACKUP_PROVIDER":      "openai_compat",
		"SYNTHWERK_LLM_FALLBACK_BACKUP_BASE_URL":      "https://llm.example.eu/v1",
		"SYNTHWERK_LLM_FALLBACK_BACKUP_API_KEY":       "sk-fallback-canary",
		"SYNTHWERK_LLM_FALLBACK_BACKUP_REGION":        "eu",
		"SYNTHWERK_LLM_FALLBACK_BACKUP_MODEL_ALIASES": "default=mistral-small",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// Load - fallback chain (#56): off by default; one EU fallback is read in order.
func TestLoadFallbacks(t *testing.T) {
	c, err := config.Load(envOf(nil))
	if err != nil || len(c.Fallbacks) != 0 || c.Region != config.RegionEU || c.AllowNonEU ||
		c.FirstTokenTimeout.Seconds() != 30 || c.BreakerFailures != 3 || c.BreakerOpen.Seconds() != 30 {
		t.Fatalf("defaults: %+v err %v", c, err)
	}
	c, err = config.Load(envOf(fallbackEnv(nil)))
	if err != nil || len(c.Fallbacks) != 1 {
		t.Fatalf("c=%+v err=%v", c.Fallbacks, err)
	}
	f := c.Fallbacks[0]
	if f.Name != "backup" || f.Provider != "openai_compat" || f.Region != "eu" || f.APIKey != "sk-fallback-canary" ||
		len(f.Aliases) != 1 || f.Aliases["default"] != "mistral-small" {
		t.Fatalf("fallback %+v", f)
	}
	c, err = config.Load(envOf(fallbackEnv(map[string]string{
		"SYNTHWERK_LLM_FALLBACK_BACKUP_REGION": "non_eu", "SYNTHWERK_LLM_ALLOW_NON_EU": "true",
	})))
	if err != nil || !c.AllowNonEU || c.Fallbacks[0].Region != "non_eu" {
		t.Fatalf("opt-in: %+v err %v", c.Fallbacks, err)
	}
}

// Load - fallback chain (#56): anything unclear refuses the start.
func TestLoadFallbacksRefuse(t *testing.T) {
	cases := map[string]struct {
		env  map[string]string
		want string
	}{
		"non-EU without opt-in":   {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACK_BACKUP_REGION": "non_eu"}), `fallback "backup" is non_eu`},
		"non-EU opt-in not bool":  {fallbackEnv(map[string]string{"SYNTHWERK_LLM_ALLOW_NON_EU": "yes please"}), "ALLOW_NON_EU must be true or false"},
		"primary non-EU":          {map[string]string{"SYNTHWERK_LLM_REGION": "non_eu"}, "REGION is non_eu"},
		"primary bad region":      {map[string]string{"SYNTHWERK_LLM_REGION": "us"}, `REGION="us" must be`},
		"openai_compat no region": {map[string]string{"SYNTHWERK_LLM_PROVIDER": "openai_compat", "SYNTHWERK_LLM_BASE_URL": "https://x.eu/v1"}, "SYNTHWERK_LLM_REGION is required"},
		"no region":               {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACK_BACKUP_REGION": ""}), "FALLBACK_BACKUP_REGION is required"},
		"unknown alias":           {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACK_BACKUP_MODEL_ALIASES": "large=x"}), "with an alias of SYNTHWERK_LLM_MODEL_ALIASES"},
		"no alias":                {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACK_BACKUP_MODEL_ALIASES": ""}), "must name at least one alias"},
		"bad name":                {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACKS": "Back-Up"}), `"Back-Up" must be a name like backup`},
		"name of the primary":     {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACKS": "ollama"}), `"ollama" is a duplicate name or the primary's`},
		"name twice":              {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACKS": "backup,backup"}), `"backup" is a duplicate name or the primary's`},
		"unknown provider":        {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACK_BACKUP_PROVIDER": "anthropic"}), "FALLBACK_BACKUP_PROVIDER=\"anthropic\" must be one of"},
		"relative url":            {fallbackEnv(map[string]string{"SYNTHWERK_LLM_FALLBACK_BACKUP_BASE_URL": "llm/v1"}), "FALLBACK_BACKUP_BASE_URL must be an absolute"},
		"fake in prd": {fallbackEnv(map[string]string{
			"SYNTHWERK_LLM_FALLBACK_BACKUP_PROVIDER": "fake", "SYNTHWERK_PLATFORM_ENV": "prd",
		}), "fake provider is refused in prd"},
		"zero first token timeout": {map[string]string{"SYNTHWERK_LLM_FIRST_TOKEN_TIMEOUT": "0s"}, "FIRST_TOKEN_TIMEOUT"},
		"zero breaker failures":    {map[string]string{"SYNTHWERK_LLM_BREAKER_FAILURES": "0"}, "BREAKER_FAILURES"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := config.Load(envOf(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sk-fallback-canary") {
				t.Fatalf("the key is in the error: %v", err)
			}
		})
	}
}
