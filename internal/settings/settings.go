// Package settings holds the AI settings of an org (synthwerk#39): the model choice,
// the system prompt, the input limit, a token cap per user and the temperature.
//
// The service sets the bounds, never the org: the model aliases come from
// SYNTHWERK_LLM_MODEL_ALIASES (the operator's allow-list), the input limit from
// SYNTHWERK_LLM_MAX_INPUT_CHARS, and the token cap from the org's plan (identity
// entitlements, #31). Normalize and Apply keep stored settings inside the bounds
// also after the operator changed them, so an org can never reach a model outside
// the allow-list.
package settings

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"
)

// Limits of the contract (openapi/llm AiSettings).
const (
	// MaxSystemPromptChars is the max length of the system prompt in characters.
	MaxSystemPromptChars = 4000
	// MaxTemperature is the max sampling temperature.
	MaxTemperature = 2.0
	// MaxModels is the max number of allowed models.
	MaxModels = 32
	// MaxUserTokens is the max token cap per user per day.
	MaxUserTokens = 1_000_000_000_000
)

// Names of the settings, as the event's "changed" list names them.
const (
	FieldModel         = "model"
	FieldAllowedModels = "allowed_models"
	FieldSystemPrompt  = "system_prompt"
	FieldMaxInputChars = "max_input_chars"
	FieldUserTokens    = "user_tokens_per_day" //nolint:gosec // a setting name, not a credential
	FieldTemperature   = "temperature"
)

// Settings of one org. Version 0: never saved, the defaults.
type Settings struct {
	// Model is the default alias of a new conversation.
	Model string
	// AllowedModels are the aliases the users of the org may pick. Holds Model.
	AllowedModels []string
	// SystemPrompt is plain text. Empty: the service prompt.
	SystemPrompt string
	// MaxInputChars is the max length of one user message.
	MaxInputChars int
	// UserTokensPerDay caps the tokens of one user per UTC day below the plan. 0: the plan only.
	UserTokensPerDay int64
	// Temperature of the model. Nil: the model default.
	Temperature *float64
	// Version goes up by one per saved change.
	Version int64
	// UpdatedAt is the time of the last saved change. Zero with Version 0.
	UpdatedAt time.Time
}

// Service is what the operator configured: the bounds of every org.
type Service struct {
	// Aliases maps a model alias to the provider model name.
	Aliases map[string]string
	// DefaultAlias is the model of a new conversation without settings.
	DefaultAlias string
	// SystemPrompt is the service prompt. Empty: none.
	SystemPrompt string
	// MaxInputChars is the input limit of the service.
	MaxInputChars int
	// ReserveTokens is booked per answer (quota). A cap at or below it refuses every send.
	ReserveTokens int
}

// Models returns the aliases of the service, sorted.
func (sv Service) Models() []string {
	return slices.Sorted(maps.Keys(sv.Aliases))
}

// Defaults are the settings of an org that never saved any.
func (sv Service) Defaults() Settings {
	return Settings{
		Model:         sv.DefaultAlias,
		AllowedModels: sv.Models(),
		MaxInputChars: sv.MaxInputChars,
	}
}

// Normalize returns s inside the current service bounds: aliases the operator removed
// are dropped, the input limit is at most the service limit. Nil s: the defaults.
func (sv Service) Normalize(s *Settings) Settings {
	if s == nil {
		return sv.Defaults()
	}
	out := *s
	out.AllowedModels = nil
	for _, m := range s.AllowedModels {
		if _, ok := sv.Aliases[m]; ok && !slices.Contains(out.AllowedModels, m) {
			out.AllowedModels = append(out.AllowedModels, m)
		}
	}
	if _, ok := sv.Aliases[out.Model]; !ok || !slices.Contains(out.AllowedModels, out.Model) {
		// The default model is gone: the service default, and it stays allowed.
		out.Model = sv.DefaultAlias
		if !slices.Contains(out.AllowedModels, out.Model) {
			out.AllowedModels = append(out.AllowedModels, out.Model)
		}
	}
	slices.Sort(out.AllowedModels)
	if out.MaxInputChars < 1 || out.MaxInputChars > sv.MaxInputChars {
		out.MaxInputChars = sv.MaxInputChars
	}
	return out
}

// Applied is what a create or send of the org uses.
type Applied struct {
	// DefaultModel is the alias of a new conversation without a choice.
	DefaultModel string
	allowed      []string
	// SystemPrompt goes first in every model call. Empty: none.
	SystemPrompt string
	// MaxInputChars is the max length of one user message.
	MaxInputChars int
	// UserTokensPerDay caps the user quota. 0: no cap.
	UserTokensPerDay int64
	// Temperature of the model. Nil: the model default.
	Temperature *float64
}

// Allows reports whether the org may use alias. Always false for an alias the
// service does not offer.
func (a Applied) Allows(alias string) bool { return slices.Contains(a.allowed, alias) }

// Apply turns stored settings (nil: none) into what a create or send uses.
func (sv Service) Apply(s *Settings) Applied {
	n := sv.Normalize(s)
	prompt := n.SystemPrompt
	if prompt == "" {
		prompt = sv.SystemPrompt
	}
	return Applied{
		DefaultModel:     n.Model,
		allowed:          n.AllowedModels,
		SystemPrompt:     prompt,
		MaxInputChars:    n.MaxInputChars,
		UserTokensPerDay: n.UserTokensPerDay,
		Temperature:      n.Temperature,
	}
}

// Cap returns the smaller of limit and cap; 0 in either means "no bound there".
func Cap(limit, capTokens int64) int64 {
	switch {
	case capTokens <= 0:
		return limit
	case limit <= 0:
		return capTokens
	default:
		return min(limit, capTokens)
	}
}

// FieldError names the setting that is out of bounds. Its text holds no value of
// the system prompt.
type FieldError struct {
	Field  string
	Detail string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Detail }

// Validate checks s against the service and the plan's per-user token limit (0: no
// limit). The first broken rule wins.
func (sv Service) Validate(s Settings, planUserTokens int64) error {
	if _, ok := sv.Aliases[s.Model]; !ok {
		return &FieldError{FieldModel, "is not a model alias of the service."}
	}
	if len(s.AllowedModels) < 1 || len(s.AllowedModels) > MaxModels {
		return &FieldError{FieldAllowedModels, fmt.Sprintf("must hold 1 to %d aliases.", MaxModels)}
	}
	seen := map[string]bool{}
	for _, m := range s.AllowedModels {
		if _, ok := sv.Aliases[m]; !ok {
			return &FieldError{FieldAllowedModels, "holds an alias the service does not offer."}
		}
		if seen[m] {
			return &FieldError{FieldAllowedModels, "holds an alias twice."}
		}
		seen[m] = true
	}
	if !seen[s.Model] {
		return &FieldError{FieldAllowedModels, "must hold model."}
	}
	if err := checkPrompt(s.SystemPrompt); err != nil {
		return err
	}
	if s.MaxInputChars < 1 || s.MaxInputChars > sv.MaxInputChars {
		return &FieldError{FieldMaxInputChars, "must be 1 to " + strconv.Itoa(sv.MaxInputChars) + "."}
	}
	switch {
	case s.UserTokensPerDay < 0 || s.UserTokensPerDay > MaxUserTokens:
		return &FieldError{FieldUserTokens, "is out of range."}
	case planUserTokens > 0 && s.UserTokensPerDay > planUserTokens:
		return &FieldError{FieldUserTokens, "must be at most the plan limit of " + strconv.FormatInt(planUserTokens, 10) + "."}
	case s.UserTokensPerDay > 0 && s.UserTokensPerDay <= int64(sv.ReserveTokens):
		// The send books the reserve before the model call: a cap at or below it refuses every send.
		return &FieldError{FieldUserTokens, "must be 0 or above " + strconv.Itoa(sv.ReserveTokens) + " (the reserve of one answer)."}
	}
	if t := s.Temperature; t != nil && (math.IsNaN(*t) || *t < 0 || *t > MaxTemperature) {
		return &FieldError{FieldTemperature, "must be 0 to 2."}
	}
	return nil
}

// checkPrompt allows plain text: printable characters, line breaks and tabs.
func checkPrompt(p string) error {
	if !utf8.ValidString(p) {
		return &FieldError{FieldSystemPrompt, "must be UTF-8 text."}
	}
	if utf8.RuneCountInString(p) > MaxSystemPromptChars {
		return &FieldError{FieldSystemPrompt, "must be at most " + strconv.Itoa(MaxSystemPromptChars) + " characters."}
	}
	for _, r := range p {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return &FieldError{FieldSystemPrompt, "must be plain text: no control characters other than line breaks and tabs."}
		}
	}
	return nil
}

// Changed lists the settings that differ between prev and next, in a fixed order.
// The allowed models compare as sets.
func Changed(prev, next Settings) []string {
	var out []string
	if prev.Model != next.Model {
		out = append(out, FieldModel)
	}
	a, b := slices.Sorted(slices.Values(prev.AllowedModels)), slices.Sorted(slices.Values(next.AllowedModels))
	if !slices.Equal(a, b) {
		out = append(out, FieldAllowedModels)
	}
	if prev.SystemPrompt != next.SystemPrompt {
		out = append(out, FieldSystemPrompt)
	}
	if prev.MaxInputChars != next.MaxInputChars {
		out = append(out, FieldMaxInputChars)
	}
	if prev.UserTokensPerDay != next.UserTokensPerDay {
		out = append(out, FieldUserTokens)
	}
	if (prev.Temperature == nil) != (next.Temperature == nil) ||
		(prev.Temperature != nil && *prev.Temperature != *next.Temperature) {
		out = append(out, FieldTemperature)
	}
	return out
}
