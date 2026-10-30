package settings_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/VelimirMueller/synthwerk-llm/internal/settings"
)

var svc = settings.Service{
	Aliases:       map[string]string{"default": "m1", "small": "m2", "large": "m3"},
	DefaultAlias:  "default",
	SystemPrompt:  "service prompt",
	MaxInputChars: 8000,
	ReserveTokens: 1024,
}

func ptr[T any](v T) *T { return &v }

func valid() settings.Settings {
	return settings.Settings{Model: "small", AllowedModels: []string{"default", "small"}, SystemPrompt: "Be brief.\nUse German.", MaxInputChars: 2000, UserTokensPerDay: 20000, Temperature: ptr(0.3)}
}

func TestValidate(t *testing.T) {
	t.Run("Validate - settings inside every bound - no error", func(t *testing.T) {
		if err := svc.Validate(valid(), 50000); err != nil {
			t.Fatal(err)
		}
		s := valid()
		s.UserTokensPerDay, s.Temperature, s.SystemPrompt = 0, nil, ""
		if err := svc.Validate(s, 0); err != nil {
			t.Fatal(err)
		}
	})
	cases := []struct {
		name, field string
		plan        int64
		edit        func(*settings.Settings)
	}{
		{"model outside the service allow-list", settings.FieldModel, 0, func(s *settings.Settings) { s.Model = "gpt-4o" }},
		{"allowed model outside the service allow-list", settings.FieldAllowedModels, 0, func(s *settings.Settings) { s.AllowedModels = []string{"small", "llama"} }},
		{"no allowed model", settings.FieldAllowedModels, 0, func(s *settings.Settings) { s.AllowedModels = nil }},
		{"alias twice", settings.FieldAllowedModels, 0, func(s *settings.Settings) { s.AllowedModels = []string{"small", "small"} }},
		{"model not in allowed models", settings.FieldAllowedModels, 0, func(s *settings.Settings) { s.AllowedModels = []string{"default"} }},
		{"prompt with a control character", settings.FieldSystemPrompt, 0, func(s *settings.Settings) { s.SystemPrompt = "hi\x1b[31m" }},
		{"prompt with a line separator", settings.FieldSystemPrompt, 0, func(s *settings.Settings) { s.SystemPrompt = "a b" }},
		{"prompt not UTF-8", settings.FieldSystemPrompt, 0, func(s *settings.Settings) { s.SystemPrompt = "\xff" }},
		{"prompt of 4001 characters", settings.FieldSystemPrompt, 0, func(s *settings.Settings) { s.SystemPrompt = strings.Repeat("ä", 4001) }},
		{"input limit 0", settings.FieldMaxInputChars, 0, func(s *settings.Settings) { s.MaxInputChars = 0 }},
		{"input limit above the service", settings.FieldMaxInputChars, 0, func(s *settings.Settings) { s.MaxInputChars = 8001 }},
		{"cap above the plan", settings.FieldUserTokens, 10000, func(s *settings.Settings) { s.UserTokensPerDay = 10001 }},
		{"cap at the answer reserve", settings.FieldUserTokens, 0, func(s *settings.Settings) { s.UserTokensPerDay = 1024 }},
		{"negative cap", settings.FieldUserTokens, 0, func(s *settings.Settings) { s.UserTokensPerDay = -1 }},
		{"temperature above 2", settings.FieldTemperature, 0, func(s *settings.Settings) { s.Temperature = ptr(2.01) }},
		{"negative temperature", settings.FieldTemperature, 0, func(s *settings.Settings) { s.Temperature = ptr(-0.1) }},
	}
	for _, c := range cases {
		t.Run("Validate - "+c.name+" - FieldError names "+c.field, func(t *testing.T) {
			s := valid()
			c.edit(&s)
			err := svc.Validate(s, c.plan)
			var fe *settings.FieldError
			if !errors.As(err, &fe) || fe.Field != c.field {
				t.Fatalf("got %v, want a FieldError for %s", err, c.field)
			}
			if strings.Contains(err.Error(), "\x1b") || strings.Contains(err.Error(), "ä") {
				t.Fatal("the error holds prompt text")
			}
		})
	}
	t.Run("Validate - prompt with 4000 characters, tabs and CRLF - no error", func(t *testing.T) {
		s := valid()
		s.SystemPrompt = strings.Repeat("ä", 3996) + "\t\r\n."
		if err := svc.Validate(s, 0); err != nil {
			t.Fatal(err)
		}
	})
}

func TestNormalizeAndApply(t *testing.T) {
	t.Run("Defaults - no settings - default alias, every alias sorted, service input limit, no cap", func(t *testing.T) {
		d := svc.Normalize(nil)
		if d.Model != "default" || !slices.Equal(d.AllowedModels, []string{"default", "large", "small"}) || d.MaxInputChars != 8000 || d.UserTokensPerDay != 0 || d.Version != 0 {
			t.Fatalf("got %+v", d)
		}
	})
	t.Run("Normalize - operator removed the org's model - service default, still allowed", func(t *testing.T) {
		s := settings.Settings{Model: "gone", AllowedModels: []string{"gone", "small"}, MaxInputChars: 9000, Version: 4}
		n := svc.Normalize(&s)
		if n.Model != "default" || !slices.Equal(n.AllowedModels, []string{"default", "small"}) || n.MaxInputChars != 8000 || n.Version != 4 {
			t.Fatalf("got %+v", n)
		}
	})
	t.Run("Apply - org settings - org prompt, org limit, only allowed aliases", func(t *testing.T) {
		s := valid()
		a := svc.Apply(&s)
		if a.DefaultModel != "small" || a.SystemPrompt != s.SystemPrompt || a.MaxInputChars != 2000 || a.UserTokensPerDay != 20000 || *a.Temperature != 0.3 {
			t.Fatalf("got %+v", a)
		}
		if !a.Allows("default") || a.Allows("large") || a.Allows("gpt-4o") {
			t.Fatal("Allows does not follow allowed_models")
		}
	})
	t.Run("Apply - empty org prompt - the service prompt", func(t *testing.T) {
		s := valid()
		s.SystemPrompt = ""
		if a := svc.Apply(&s); a.SystemPrompt != "service prompt" {
			t.Fatalf("got %q", a.SystemPrompt)
		}
	})
}

func TestCap(t *testing.T) {
	for _, c := range []struct{ limit, cap, want int64 }{
		{50000, 0, 50000}, {0, 20000, 20000}, {50000, 20000, 20000}, {10000, 20000, 10000}, {0, 0, 0},
	} {
		if got := settings.Cap(c.limit, c.cap); got != c.want {
			t.Errorf("Cap(%d, %d) = %d, want %d", c.limit, c.cap, got, c.want)
		}
	}
}

func TestChanged(t *testing.T) {
	t.Run("Changed - same values, other order of models - nothing", func(t *testing.T) {
		a, b := valid(), valid()
		b.AllowedModels = []string{"small", "default"}
		b.Temperature = ptr(0.3)
		if got := settings.Changed(a, b); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("Changed - every setting - every name in fixed order", func(t *testing.T) {
		a := valid()
		b := settings.Settings{Model: "default", AllowedModels: []string{"default"}, SystemPrompt: "x", MaxInputChars: 1, UserTokensPerDay: 2000}
		want := []string{"model", "allowed_models", "system_prompt", "max_input_chars", "user_tokens_per_day", "temperature"}
		if got := settings.Changed(a, b); !slices.Equal(got, want) {
			t.Fatalf("got %v", got)
		}
	})
	t.Run("Changed - temperature value only - temperature", func(t *testing.T) {
		a, b := valid(), valid()
		b.Temperature = ptr(0.4)
		if got := settings.Changed(a, b); !slices.Equal(got, []string{"temperature"}) {
			t.Fatalf("got %v", got)
		}
	})
}
