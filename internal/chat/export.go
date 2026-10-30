package chat

import (
	"bufio"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/problem"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// exportMe serves GET /v1/me/export (synthwerk#41): every conversation, message and
// own quota row of the token sub, in every org.
//
//   - Owner = token sub only. The token org does not limit the export.
//   - A widget token (claim synthwerk:origin) gets 403: the export is for the user.
//   - Bound: the body streams row by row from one database snapshot, so memory
//     stays one row. The contract caps each list at 100000 items; a user above
//     the cap gets 500 before the first byte (event llm.export.too_large).
//   - A failure after the first byte aborts the connection: the client gets
//     broken JSON, never a short export that looks complete.
func (h *Handler) exportMe(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Subject == "" {
		// auth.Middleware never lets this through; keep the refusal anyway.
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem.Write(w, r, http.StatusUnauthorized, auth.CodeUnauthenticated, "", 0)
		return
	}
	if _, widget := p.Raw[auth.ClaimOrigin]; widget {
		problem.Write(w, r, http.StatusForbidden, problem.CodeForbidden, "", 0)
		return
	}
	ew := &exportWriter{w: w}
	err := h.store.Export(r.Context(), p.Subject, ew)
	if err == nil {
		err = ew.end()
	}
	if err == nil {
		h.log.InfoContext(r.Context(), "own data exported",
			slog.String("event.name", "llm.export.served"),
			slog.Int("synthwerk.conversations", ew.convs),
			slog.Int("synthwerk.messages", ew.total),
			slog.Int("synthwerk.quota_rows", ew.quotas))
		return
	}
	if !ew.started {
		event := "llm.export.failed"
		if errors.Is(err, store.ErrExportTooLarge) {
			event = "llm.export.too_large"
		}
		h.internal(w, r, event, err)
		return
	}
	h.log.ErrorContext(r.Context(), "export aborted after the first byte",
		slog.String("event.name", "llm.export.aborted"), slog.String("exception.type", errType(err)))
	panic(http.ErrAbortHandler) // net/http closes the connection without a log line
}

// exportWriter writes LlmExport as JSON while the store reads the rows. The 200
// header goes out with the first row (or at the end), so an error before it still
// gets a problem response.
type exportWriter struct {
	w       http.ResponseWriter
	bw      *bufio.Writer
	started bool
	inConv  bool // a conversation's message list is open
	inQuota bool // the quota_usage list is open
	convs   int
	msgs    int // messages of the open conversation
	total   int // messages of all conversations
	quotas  int
}

// convHead is ExportConversation without messages: the writer streams those.
type convHead struct {
	ID        uuid.UUID `json:"id"`
	OrgID     string    `json:"org_id"`
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"created_at"`
}

func (e *exportWriter) start() {
	if e.started {
		return
	}
	e.started = true
	h := e.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	e.w.WriteHeader(http.StatusOK)
	e.bw = bufio.NewWriterSize(e.w, 32<<10)
	_, _ = e.bw.WriteString(`{"conversations":[`)
}

func (e *exportWriter) closeConv() {
	if e.inConv {
		_, _ = e.bw.WriteString("]}")
		e.inConv = false
	}
}

func (e *exportWriter) raw(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = e.bw.Write(b)
	return err
}

// Conversation implements store.ExportSink.
func (e *exportWriter) Conversation(c store.ExportConversation) error {
	id, err := uuid.Parse(c.ID)
	if err != nil {
		return err
	}
	e.start()
	e.closeConv()
	if e.convs > 0 {
		_ = e.bw.WriteByte(',')
	}
	b, err := json.Marshal(convHead{ID: id, OrgID: c.Tenant, Model: c.ModelAlias, CreatedAt: c.CreatedAt.UTC()})
	if err != nil {
		return err
	}
	// {"id":...,"created_at":"..."} → {"id":...,"created_at":"...","messages":[
	_, _ = e.bw.Write(b[:len(b)-1])
	_, err = e.bw.WriteString(`,"messages":[`)
	e.inConv, e.msgs = true, 0
	e.convs++
	return err
}

// Message implements store.ExportSink.
func (e *exportWriter) Message(m store.ExportMessage) error {
	if !e.inConv {
		return errors.New("export: message outside a conversation")
	}
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return err
	}
	if e.msgs > 0 {
		_ = e.bw.WriteByte(',')
	}
	e.msgs++
	e.total++
	return e.raw(llm.ExportMessage{
		Id:        id,
		Role:      llm.MessageRole(m.Role),
		Content:   m.Content,
		Status:    llm.MessageStatus(m.Status),
		TokensIn:  m.TokensIn,
		TokensOut: m.TokensOut,
		CreatedAt: m.CreatedAt.UTC(),
	})
}

// Quota implements store.ExportSink.
func (e *exportWriter) Quota(q store.ExportQuota) error {
	e.openQuota()
	if e.quotas > 0 {
		_ = e.bw.WriteByte(',')
	}
	e.quotas++
	return e.raw(llm.ExportQuotaUsage{
		OrgId:       q.Tenant,
		WindowStart: openapi_types.Date{Time: q.WindowStart},
		Used:        q.Used,
	})
}

func (e *exportWriter) openQuota() {
	e.start()
	if e.inQuota {
		return
	}
	e.closeConv()
	_, _ = e.bw.WriteString(`],"quota_usage":[`)
	e.inQuota = true
}

// end closes the JSON and flushes it.
func (e *exportWriter) end() error {
	e.openQuota()
	_, _ = e.bw.WriteString("]}\n")
	return e.bw.Flush()
}
