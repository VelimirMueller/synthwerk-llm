// Package aisettings serves GET and PUT /v1/ai-settings (synthwerk#39): the AI
// settings of the token's org, for its owners and admins.
//
// Refusal order. Identity is asked last, the store after it:
//  1. auth.Middleware: no valid token → 401.
//  2. a widget token (claim synthwerk:origin), or no role owner or admin → 403.
//  3. PUT: body not valid → 400.
//  4. identity GET /v1/me/entitlements with the caller's token: not an active
//     member → 403; identity down or not configured → 503 settings_unavailable.
//  5. PUT: a value outside the service bounds or the plan → 400; another stored
//     version → 409 version_conflict. A change writes the row and
//     dev.synthwerk.llm.ai_settings.changed.v1 in one transaction.
//
// Logs carry metadata only: never the system prompt.
package aisettings

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/problem"
	"github.com/VelimirMueller/synthwerk-llm/internal/settings"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// EventType is the fact of a change.
const EventType = "dev.synthwerk.llm.ai_settings.changed.v1"

const dataSchema = "https://schemas.synthwerk.dev/events/llm/ai_settings.changed.v1.json"

// maxBody bounds the PUT body: 4000 prompt characters escaped as \uXXXX at worst,
// plus the other fields.
const maxBody = 64 << 10

// Store is the persistence the handler needs. *store.Store implements it.
type Store interface {
	OrgSettings(ctx context.Context, o store.Owner) (*settings.Settings, error)
	SaveOrgSettings(ctx context.Context, o store.Owner, next settings.Settings, expect int64, build store.SettingsChange) (settings.Settings, bool, error)
}

// Plans reads the caller's plan and checks the membership. *identity.Client implements it.
type Plans interface {
	MyEntitlements(ctx context.Context, bearer string) (identity.Plan, error)
}

// Handler serves the settings routes.
type Handler struct {
	svc    settings.Service
	store  Store
	plans  Plans
	source string
	log    *slog.Logger
}

// New returns the handler. plans nil: identity is not configured, every call
// answers 503 settings_unavailable. source is the CloudEvents source.
func New(svc settings.Service, st Store, plans Plans, source string, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, store: st, plans: plans, source: source, log: logger}
}

// Register adds the routes to mux. authn must wrap auth.Middleware and take user
// tokens only (app: no widget tokens, #30).
func (h *Handler) Register(mux *http.ServeMux, authn func(http.Handler) http.Handler) {
	mux.Handle("GET /v1/ai-settings", authn(http.HandlerFunc(h.get)))
	mux.Handle("PUT /v1/ai-settings", authn(http.HandlerFunc(h.put)))
}

// event is the data of llm.ai_settings.changed.v1. No system prompt text.
type event struct {
	OrgID             string   `json:"org_id"`
	Version           int64    `json:"version"`
	Changed           []string `json:"changed"`
	Model             string   `json:"model"`
	AllowedModels     []string `json:"allowed_models"`
	MaxInputChars     int      `json:"max_input_chars"`
	UserTokensPerDay  int64    `json:"user_tokens_per_day"`
	SystemPromptChars int      `json:"system_prompt_chars"`
	Temperature       *float64 `json:"temperature,omitempty"`
}

// request is UpdateAiSettingsRequest. Pointers find missing required fields;
// temperature stays float64 (the generated type is float32 and would round).
type request struct {
	Version          *int64    `json:"version"`
	Model            *string   `json:"model"`
	AllowedModels    *[]string `json:"allowed_models"`
	SystemPrompt     *string   `json:"system_prompt"`
	MaxInputChars    *int      `json:"max_input_chars"`
	UserTokensPerDay *int64    `json:"user_tokens_per_day"`
	Temperature      *float64  `json:"temperature"`
}

// admin checks the caller: a user token (no widget origin claim) with the org role
// owner or admin. It writes the refusal and returns ok false.
func admin(w http.ResponseWriter, r *http.Request) (*auth.Principal, store.Owner, string, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Subject == "" || p.OrgID == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem.Write(w, r, http.StatusUnauthorized, auth.CodeUnauthenticated, "", 0)
		return nil, store.Owner{}, "", false
	}
	if _, widget := p.Raw[auth.ClaimOrigin]; widget || (!p.HasRole(auth.RoleOwner) && !p.HasRole(auth.RoleAdmin)) {
		problem.Write(w, r, http.StatusForbidden, problem.CodeForbidden, "", 0)
		return nil, store.Owner{}, "", false
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" {
		// auth.Middleware read the same header; keep the refusal anyway.
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem.Write(w, r, http.StatusUnauthorized, auth.CodeUnauthenticated, "", 0)
		return nil, store.Owner{}, "", false
	}
	return p, store.Owner{Tenant: p.OrgID, User: p.Subject}, bearer, true
}

// plan asks identity. It writes the refusal and returns ok false.
func (h *Handler) plan(w http.ResponseWriter, r *http.Request, bearer string) (identity.Plan, bool) {
	if h.plans == nil {
		h.log.WarnContext(r.Context(), "AI settings need identity",
			slog.String("event.name", "llm.settings.unavailable"), slog.String("exception.type", "not_configured"))
		problem.Write(w, r, http.StatusServiceUnavailable, problem.CodeSettingsUnavailable, "", 30)
		return identity.Plan{}, false
	}
	pl, err := h.plans.MyEntitlements(r.Context(), bearer)
	switch {
	case errors.Is(err, identity.ErrDenied):
		problem.Write(w, r, http.StatusForbidden, problem.CodeForbidden, "", 0)
		return identity.Plan{}, false
	case err != nil:
		h.log.WarnContext(r.Context(), "identity did not answer",
			slog.String("event.name", "llm.settings.unavailable"), slog.String("exception.type", "identity_unavailable"))
		problem.Write(w, r, http.StatusServiceUnavailable, problem.CodeSettingsUnavailable, "", 5)
		return identity.Plan{}, false
	}
	return pl, true
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	_, o, bearer, ok := admin(w, r)
	if !ok {
		return
	}
	pl, ok := h.plan(w, r, bearer)
	if !ok {
		return
	}
	s, err := h.store.OrgSettings(r.Context(), o)
	if err != nil {
		h.internal(w, r, "llm.settings.read_failed")
		return
	}
	writeJSON(w, http.StatusOK, h.body(h.svc.Normalize(s), pl))
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request) {
	p, o, bearer, ok := admin(w, r)
	if !ok {
		return
	}
	next, expect, detail := decode(w, r)
	if detail != "" {
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, detail, 0)
		return
	}
	pl, ok := h.plan(w, r, bearer)
	if !ok {
		return
	}
	if err := h.svc.Validate(next, pl.UserTokensPerDay); err != nil {
		var fe *settings.FieldError
		detail := "A setting is out of bounds."
		if errors.As(err, &fe) {
			detail = fe.Error()
		}
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, detail, 0)
		return
	}
	ctx := r.Context()
	var changed []string
	saved, didChange, err := h.store.SaveOrgSettings(ctx, o, next, expect, func(prev *settings.Settings, version int64) (*cloudevents.Event, error) {
		// Compare with what the org had in effect: the defaults when nothing is stored.
		changed = settings.Changed(h.svc.Normalize(prev), next)
		if len(changed) == 0 {
			return nil, nil
		}
		data := event{
			OrgID: pl.OrgID, Version: version, Changed: changed,
			Model: next.Model, AllowedModels: next.AllowedModels, MaxInputChars: next.MaxInputChars,
			UserTokensPerDay: next.UserTokensPerDay, SystemPromptChars: len([]rune(next.SystemPrompt)),
			Temperature: next.Temperature,
		}
		ev, err := cloudevents.New(ctx, EventType, h.source, data,
			cloudevents.WithSubject(pl.OrgID),
			cloudevents.WithOrg(pl.OrgID),
			cloudevents.WithActor("user:"+p.Subject),
			cloudevents.WithDataSchema(dataSchema))
		if err != nil {
			return nil, err
		}
		return &ev, nil
	})
	if errors.Is(err, store.ErrVersionConflict) {
		problem.Write(w, r, http.StatusConflict, problem.CodeVersionConflict, "The settings changed. Read them again.", 0)
		return
	}
	if err != nil {
		h.internal(w, r, "llm.settings.save_failed")
		return
	}
	if didChange {
		h.log.InfoContext(r.Context(), "AI settings changed",
			slog.String("event.name", "llm.settings.changed"),
			slog.Int64("synthwerk.version", saved.Version),
			slog.String("synthwerk.changed", strings.Join(changed, ",")))
	}
	var out settings.Settings
	if saved.Version == 0 {
		out = h.svc.Normalize(nil) // the same values as the defaults: nothing stored
	} else {
		out = h.svc.Normalize(&saved)
	}
	writeJSON(w, http.StatusOK, h.body(out, pl))
}

// decode reads the PUT body strictly. detail is "" when it is valid.
func decode(w http.ResponseWriter, r *http.Request) (settings.Settings, int64, string) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	var req request
	if err := dec.Decode(&req); err != nil {
		return settings.Settings{}, 0, "The body is not a valid request."
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return settings.Settings{}, 0, "The body is not a valid request."
	}
	if req.Version == nil || req.Model == nil || req.AllowedModels == nil || req.SystemPrompt == nil ||
		req.MaxInputChars == nil || req.UserTokensPerDay == nil {
		return settings.Settings{}, 0, "A required field is missing."
	}
	if *req.Version < 0 {
		return settings.Settings{}, 0, "version must be 0 or more."
	}
	return settings.Settings{
		Model: *req.Model, AllowedModels: slices.Sorted(slices.Values(*req.AllowedModels)), SystemPrompt: *req.SystemPrompt,
		MaxInputChars: *req.MaxInputChars, UserTokensPerDay: *req.UserTokensPerDay, Temperature: req.Temperature,
	}, *req.Version, ""
}

func (h *Handler) body(s settings.Settings, pl identity.Plan) llm.AiSettings {
	out := llm.AiSettings{
		Model:            s.Model,
		AllowedModels:    s.AllowedModels,
		SystemPrompt:     s.SystemPrompt,
		MaxInputChars:    int32(s.MaxInputChars), //nolint:gosec // 1..8000, checked
		UserTokensPerDay: s.UserTokensPerDay,
		Version:          s.Version,
		Bounds: llm.AiSettingsBounds{
			Models:               h.svc.Models(),
			MaxInputChars:        int32(h.svc.MaxInputChars), //nolint:gosec // 1..8000, config checks it
			UserTokensPerDay:     pl.UserTokensPerDay,
			SystemPromptMaxChars: settings.MaxSystemPromptChars,
			TemperatureMax:       settings.MaxTemperature,
		},
	}
	if s.Temperature != nil {
		t := float32(*s.Temperature)
		out.Temperature = &t
	}
	if s.Version > 0 {
		u := s.UpdatedAt.UTC()
		out.UpdatedAt = &u
	}
	return out
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, event string) {
	h.log.ErrorContext(r.Context(), "request failed", slog.String("event.name", event), slog.String("exception.type", "error"))
	problem.Write(w, r, http.StatusInternalServerError, problem.CodeInternal, "", 0)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
