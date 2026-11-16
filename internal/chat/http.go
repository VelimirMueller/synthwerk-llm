package chat

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/problem"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// maxBody bounds a JSON request body: 8000 characters, each escaped as \uXXXX at worst.
const maxBody = 64 << 10

// CORS answers CORS for the widget chat routes (infra#21). *cors.Policy implements it.
type CORS interface {
	// Wrap sets the CORS headers of an actual request.
	Wrap(next http.Handler) http.Handler
	// Preflight answers OPTIONS for a route with methods.
	Preflight(methods ...string) http.Handler
}

// Register adds the chat routes to mux. Both checks wrap auth.Middleware.
// chatAuthn guards the routes the widget uses: user and widget tokens (#30).
// userAuthn guards the export: user tokens only.
// c answers CORS on the widget routes only. It sits outside the token check, so
// the preflight needs no token and a 401 is readable by the widget. The export
// gets no CORS here: the edge answers for it.
func (h *Handler) Register(mux *http.ServeMux, chatAuthn, userAuthn func(http.Handler) http.Handler, c CORS) {
	widget := []struct {
		method, path string
		fn           http.HandlerFunc
	}{
		{http.MethodGet, "/v1/conversations", h.list},
		{http.MethodPost, "/v1/conversations", h.create},
		{http.MethodGet, "/v1/conversations/{id}", h.get},
		{http.MethodPost, "/v1/conversations/{id}/messages", h.send},
	}
	// One OPTIONS handler per path with every method of that path, in any order.
	methods := map[string][]string{}
	for _, rt := range widget {
		mux.Handle(rt.method+" "+rt.path, c.Wrap(chatAuthn(rt.fn)))
		methods[rt.path] = append(methods[rt.path], rt.method)
	}
	for path, ms := range methods {
		mux.Handle(http.MethodOptions+" "+path, c.Preflight(ms...))
	}
	mux.Handle("GET /v1/me/export", userAuthn(http.HandlerFunc(h.exportMe)))
}

// owner returns the principal's owner and checks the widget origin claim (#30).
// Owner = token org and sub: AI settings and the org quota apply per org, the user
// quota per sub, for a widget token too. A widget token also sets Owner.Origin: its
// conversations stay bound to that origin (#54).
// The Origin header must equal the claim, or a configured widget frame origin
// (synthwerk-widgets#33): the host page trades the token and hands it into the chat
// iframe on the widget host. The claim must still be a non-empty string.
// It writes the refusal and returns ok false.
func (h *Handler) owner(w http.ResponseWriter, r *http.Request) (*auth.Principal, store.Owner, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Subject == "" || p.OrgID == "" {
		// auth.Middleware never lets this through; keep the refusal anyway.
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem.Write(w, r, http.StatusUnauthorized, auth.CodeUnauthenticated, "", 0)
		return nil, store.Owner{}, false
	}
	o := store.Owner{Tenant: p.OrgID, User: p.Subject}
	if origin, has := p.Raw[auth.ClaimOrigin]; has {
		s, _ := origin.(string)
		if s == "" || !h.originAllowed(s, r) {
			problem.Write(w, r, http.StatusForbidden, problem.CodeForbidden, "", 0)
			return nil, store.Owner{}, false
		}
		o.Origin = s
	}
	return p, o, true
}

// originAllowed reports whether the request may use a token bound to claim: its Origin
// is the claim or a widget frame origin (exact match, as for the claim).
//
// A browser sends no Origin on a same-origin GET or HEAD. The chat frame is on the
// edge origin, so its GETs (past conversations) come without one. Such a request
// passes only when frame origins are set and the browser marks it same-origin:
// a page cannot set Sec-Fetch-Site (a forbidden header). A client outside a
// browser can send any header, the same as it can send any Origin.
func (h *Handler) originAllowed(claim string, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return len(h.cfg.WidgetFrameOrigins) > 0 &&
			(r.Method == http.MethodGet || r.Method == http.MethodHead) &&
			r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return origin == claim || slices.Contains(h.cfg.WidgetFrameOrigins, origin)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	_, o, ok := h.owner(w, r)
	if !ok {
		return
	}
	var req llm.CreateConversationRequest
	if err := decode(w, r, &req, true); err != nil {
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "The body is not a valid request.", 0)
		return
	}
	ap, err := h.applied(r.Context(), o)
	if err != nil {
		h.internal(w, r, "llm.conversation.create_failed", err)
		return
	}
	alias := ap.DefaultModel
	if req.Model != nil {
		alias = *req.Model
	}
	if !ap.Allows(alias) {
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "model is not an alias this org allows.", 0)
		return
	}
	c, err := h.store.CreateConversation(r.Context(), o, alias)
	if err != nil {
		h.internal(w, r, "llm.conversation.create_failed", err)
		return
	}
	w.Header().Set("Location", "/v1/conversations/"+c.ID)
	writeJSON(w, http.StatusCreated, toContract(c))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	_, o, ok := h.owner(w, r)
	if !ok {
		return
	}
	id, ok := conversationID(w, r)
	if !ok {
		return
	}
	c, err := h.store.GetConversation(r.Context(), o, id)
	if errors.Is(err, store.ErrNotFound) {
		problem.Write(w, r, http.StatusNotFound, problem.CodeNotFound, "", 0)
		return
	}
	if err != nil {
		h.internal(w, r, "llm.conversation.read_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, toContract(c))
}

// conversationID parses the path id. A bad id answers 404, like an unknown one.
func conversationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil || id.String() != r.PathValue("id") {
		problem.Write(w, r, http.StatusNotFound, problem.CodeNotFound, "", 0)
		return "", false
	}
	return id.String(), true
}

// decode reads a JSON body with unknown fields refused. emptyOK accepts no body.
func decode(w http.ResponseWriter, r *http.Request, dst any, emptyOK bool) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if errors.Is(err, io.EOF) && emptyOK {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON body")
	}
	return nil
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, event string, err error) {
	// Store errors name tables and SQL states, never content.
	h.log.ErrorContext(r.Context(), "request failed",
		slog.String("event.name", event), slog.String("exception.type", errType(err)))
	problem.Write(w, r, http.StatusInternalServerError, problem.CodeInternal, "", 0)
}

// errType is a short, content-free class of an error for logs.
func errType(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrNotFound):
		return "not_found"
	default:
		return "error"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func toContract(c store.Conversation) llm.Conversation {
	out := llm.Conversation{
		Id:        uuid.MustParse(c.ID),
		Model:     c.ModelAlias,
		CreatedAt: c.CreatedAt.UTC(),
		Messages:  make([]llm.Message, 0, len(c.Messages)),
	}
	for _, m := range c.Messages {
		out.Messages = append(out.Messages, llm.Message{
			Id:        uuid.MustParse(m.ID),
			Role:      llm.MessageRole(m.Role),
			Content:   m.Content,
			Status:    llm.MessageStatus(m.Status),
			CreatedAt: m.CreatedAt.UTC(),
		})
	}
	return out
}
