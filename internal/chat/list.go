package chat

import (
	"encoding/base64"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/problem"
)

// List paging (contract llm.listConversations).
const (
	listDefault   = 20
	listMax       = 50
	listCursorMax = 64
)

// list serves GET /v1/conversations: the caller's own conversations in the token
// org, newest first (synthwerk#54). A widget token sees only its own origin.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	_, o, ok := owner(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit := listDefault
	if v, has := q["limit"]; has {
		n, err := strconv.Atoi(v[0])
		if len(v) != 1 || err != nil || n < 1 || n > listMax || strconv.Itoa(n) != v[0] {
			problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "limit must be an integer from 1 to 50.", 0)
			return
		}
		limit = n
	}
	var before string
	if v, has := q["cursor"]; has {
		id, ok := decodeCursor(v)
		if !ok {
			problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "cursor is not valid.", 0)
			return
		}
		before = id
	}
	items, more, err := h.store.ListConversations(r.Context(), o, before, limit)
	if err != nil {
		h.internal(w, r, "llm.conversation.list_failed", err)
		return
	}
	page := llm.ConversationPage{Items: make([]llm.ConversationSummary, 0, len(items))}
	for _, it := range items {
		page.Items = append(page.Items, llm.ConversationSummary{
			Id:        uuid.MustParse(it.ID),
			Model:     it.ModelAlias,
			Title:     it.Title,
			CreatedAt: it.CreatedAt.UTC(),
		})
	}
	if more && len(items) > 0 {
		next := base64.RawURLEncoding.EncodeToString([]byte(items[len(items)-1].ID))
		page.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, page)
}

// decodeCursor reads one cursor value: base64url (no padding) of a canonical UUID.
func decodeCursor(v []string) (string, bool) {
	if len(v) != 1 || v[0] == "" || len(v[0]) > listCursorMax {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(v[0])
	if err != nil {
		return "", false
	}
	id, err := uuid.Parse(string(raw))
	if err != nil || id.String() != string(raw) {
		return "", false
	}
	return id.String(), true
}
