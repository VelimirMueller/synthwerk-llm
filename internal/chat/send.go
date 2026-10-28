package chat

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/llm"

	"github.com/VelimirMueller/synthwerk-llm/internal/problem"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/sse"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// finishTimeout bounds the final write after the stream, also after a client abort.
const finishTimeout = 5 * time.Second

// completed is the data of llm.message.completed.v1. Metadata only.
type completed struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	LatencyMS      int64  `json:"latency_ms"`
	TTFTMS         *int64 `json:"ttft_ms,omitempty"`
	Status         string `json:"status"`
}

// answer is what the stream loop collected.
type answer struct {
	text   []byte
	status string
	result provider.Result
	ttft   time.Duration
	pieces int
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p, o, ok := owner(w, r)
	if !ok {
		return
	}
	convID, ok := conversationID(w, r)
	if !ok {
		return
	}
	var req llm.SendMessageRequest
	if err := decode(w, r, &req, false); err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			problem.Write(w, r, http.StatusRequestEntityTooLarge, problem.CodeInputTooLarge, "", 0)
			return
		}
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "The body is not a valid request.", 0)
		return
	}
	if req.Content == "" || !utf8.ValidString(req.Content) {
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "content must be non-empty UTF-8 text.", 0)
		return
	}

	alias, err := h.store.ConversationAlias(r.Context(), o, convID)
	if errors.Is(err, store.ErrNotFound) {
		problem.Write(w, r, http.StatusNotFound, problem.CodeNotFound, "", 0)
		return
	}
	if err != nil {
		h.internal(w, r, "llm.message.start_failed", err)
		return
	}
	if req.Model != nil {
		alias = *req.Model
	}
	model, known := h.cfg.Aliases[alias]
	if !known {
		problem.Write(w, r, http.StatusBadRequest, problem.CodeInvalidRequest, "model is not a known alias.", 0)
		return
	}
	if utf8.RuneCountInString(req.Content) > h.cfg.MaxInputChars {
		problem.Write(w, r, http.StatusRequestEntityTooLarge, problem.CodeInputTooLarge, "", 0)
		return
	}
	releaseStream, ok := h.acquireStream(o)
	if !ok {
		problem.Write(w, r, http.StatusTooManyRequests, problem.CodeTooManyStreams, "", 5)
		return
	}
	defer releaseStream()
	if err := h.guard.Check(r.Context(), p, alias); err != nil {
		if errors.Is(err, quota.ErrExhausted) {
			problem.Write(w, r, http.StatusTooManyRequests, "quota_exhausted", "", 60)
			return
		}
		h.internal(w, r, "llm.quota.check_failed", err)
		return
	}

	turn, err := h.store.StartTurn(r.Context(), o, convID, req.Content)
	if errors.Is(err, store.ErrNotFound) {
		problem.Write(w, r, http.StatusNotFound, problem.CodeNotFound, "", 0)
		return
	}
	if err != nil {
		h.internal(w, r, "llm.message.start_failed", err)
		return
	}

	ans := h.stream(w, r, turn, provider.Request{Model: model, Messages: h.history(turn.History, req.Content)}, alias)
	h.finish(r.Context(), p, o, convID, turn.AssistantMessageID, alias, start, ans)
}

// stream runs the model call and writes the SSE answer. It never returns an error:
// the outcome is in the answer status.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, turn store.Turn, req provider.Request, alias string) answer {
	ans := answer{status: store.StatusFailed}
	releaseModel, ok := h.acquireModel(r.Context())
	if !ok {
		if r.Context().Err() != nil {
			ans.status = store.StatusCanceled
			return ans
		}
		problem.Write(w, r, http.StatusServiceUnavailable, problem.CodeModelUnavailable, "The model is busy. Try again later.", 5)
		return ans
	}
	defer releaseModel()

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.StreamTimeout)
	defer cancel()
	callStart := time.Now()
	st, err := h.provider.Chat(ctx, req)
	if err != nil {
		if r.Context().Err() != nil {
			ans.status = store.StatusCanceled
			return ans
		}
		problem.Write(w, r, http.StatusServiceUnavailable, problem.CodeModelUnavailable, "", 5)
		return ans
	}
	defer func() { _ = st.Close() }()

	sw, err := sse.Start(w, turn.AssistantMessageID)
	if err != nil {
		ans.status = store.StatusCanceled
		return ans
	}

	// The reader goroutine owns the provider stream; this loop owns the writer.
	type piece struct {
		text string
		err  error
	}
	pieces := make(chan piece)
	go func() {
		defer close(pieces)
		for {
			t, err := st.Next()
			select {
			case pieces <- piece{t, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	ping := time.NewTicker(h.cfg.Heartbeat)
	defer ping.Stop()

	for {
		select {
		case <-ping.C:
			if sw.Ping() != nil {
				ans.status = store.StatusCanceled
				return ans
			}
		case <-ctx.Done():
			return h.ended(r, sw, ans)
		case pc, open := <-pieces:
			if !open {
				return h.ended(r, sw, ans)
			}
			switch {
			case errors.Is(pc.err, io.EOF):
				ans.result = st.Result()
				ans.status = store.StatusCompleted
				usage := llm.Usage{InputTokens: ans.result.Usage.InputTokens, OutputTokens: ans.result.Usage.OutputTokens, Model: alias}
				if sw.Event("usage", usage) != nil {
					ans.status = store.StatusCanceled // the client left before done
					return ans
				}
				done := llm.Done{MessageId: uuid.MustParse(turn.AssistantMessageID), FinishReason: llm.DoneFinishReason(ans.result.Finish)}
				_ = sw.Event("done", done)
				return ans
			case pc.err != nil:
				if r.Context().Err() != nil {
					ans.status = store.StatusCanceled
					return ans
				}
				_ = sw.Event("error", problem.New(r, http.StatusServiceUnavailable, problem.CodeModelUnavailable, ""))
				return ans
			}
			if ans.pieces == 0 {
				ans.ttft = time.Since(callStart)
			}
			ans.pieces++
			ans.text = append(ans.text, pc.text...)
			if sw.Event("delta", llm.Delta{Text: pc.text}) != nil {
				ans.status = store.StatusCanceled
				return ans
			}
		}
	}
}

// ended handles the stream context ending: a client abort or the stream timeout.
func (*Handler) ended(r *http.Request, sw *sse.Writer, ans answer) answer {
	if r.Context().Err() != nil {
		ans.status = store.StatusCanceled
		return ans
	}
	ans.status = store.StatusFailed
	_ = sw.Event("error", problem.New(r, http.StatusServiceUnavailable, problem.CodeModelUnavailable, "The answer took too long."))
	return ans
}

// finish stores the end state and the event, also after a client abort.
func (h *Handler) finish(rctx context.Context, p *auth.Principal, o store.Owner, convID, msgID, alias string, start time.Time, ans answer) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(rctx), finishTimeout)
	defer cancel()

	usage := ans.result.Usage
	if ans.status != store.StatusCompleted {
		usage = provider.Usage{OutputTokens: provider.EstimateTokens(string(ans.text)), Estimated: true}
	}
	data := completed{
		ConversationID: convID,
		MessageID:      msgID,
		Model:          alias,
		Provider:       h.provider.Name(),
		InputTokens:    usage.InputTokens,
		OutputTokens:   usage.OutputTokens,
		LatencyMS:      time.Since(start).Milliseconds(),
		Status:         ans.status,
	}
	if ans.pieces > 0 {
		ms := ans.ttft.Milliseconds()
		data.TTFTMS = &ms
	}
	ev, err := cloudevents.New(ctx, EventType, h.cfg.Source, data,
		cloudevents.WithSubject(msgID),
		cloudevents.WithOrg(p.OrgID),
		cloudevents.WithActor("user:"+p.Subject),
		cloudevents.WithDataSchema(dataSchema))
	if err == nil {
		err = h.store.Finish(ctx, o, store.Final{
			MessageID: msgID, Content: string(ans.text), Status: ans.status,
			TokensIn: usage.InputTokens, TokensOut: usage.OutputTokens,
		}, ev)
	}
	attrs := []slog.Attr{
		slog.String("event.name", "llm.message.finished"),
		slog.String("synthwerk.message_id", msgID),
		slog.String("synthwerk.status", ans.status),
		slog.String("gen_ai.request.model", alias),
		slog.String("gen_ai.provider.name", h.provider.Name()),
		slog.Int("gen_ai.usage.input_tokens", usage.InputTokens),
		slog.Int("gen_ai.usage.output_tokens", usage.OutputTokens),
		slog.Int64("synthwerk.latency_ms", data.LatencyMS),
	}
	if err != nil {
		attrs = append(attrs, slog.String("exception.type", errType(err)))
		h.log.LogAttrs(ctx, slog.LevelError, "answer not stored", attrs...)
		return
	}
	h.log.LogAttrs(ctx, slog.LevelInfo, "answer finished", attrs...)
}
