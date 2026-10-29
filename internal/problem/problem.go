// Package problem writes RFC 9457 errors with the stable Synthwerk code of the chat
// contract (openapi/llm Problem). The detail never holds provider text or content.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Codes of the chat contract.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeInputTooLarge    = "input_too_large"
	CodeTooManyStreams   = "too_many_streams"
	CodeQuotaExhausted   = "quota_exhausted"
	CodeModelUnavailable = "model_unavailable"
	CodeInternal         = "internal_error"
)

var titles = map[string]string{
	CodeInvalidRequest:   "Invalid request",
	CodeForbidden:        "Forbidden",
	CodeNotFound:         "Not found",
	CodeInputTooLarge:    "Input too large",
	CodeTooManyStreams:   "Too many streams",
	CodeQuotaExhausted:   "Quota exhausted",
	CodeModelUnavailable: "Model unavailable",
	CodeInternal:         "Internal error",
}

// Problem is the body.
type Problem struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Detail  string `json:"detail,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
	// Quota is set for quota_exhausted only.
	Quota *Quota `json:"quota,omitempty"`
}

// Quota is the contract's QuotaExceeded: the quota that ran out. No usage numbers.
type Quota struct {
	Scope    string    `json:"scope"`
	Unit     string    `json:"unit"`
	Limit    int64     `json:"limit"`
	ResetsAt time.Time `json:"resets_at"`
}

// New builds a problem; the trace id comes from the span in ctx.
func New(r *http.Request, status int, code, detail string) Problem {
	p := Problem{
		Type:   "https://schemas.synthwerk.dev/problems/" + code,
		Title:  titles[code],
		Status: status,
		Code:   code,
		Detail: detail,
	}
	if p.Title == "" {
		p.Title = http.StatusText(status)
	}
	if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
		p.TraceID = sc.TraceID().String()
	}
	return p
}

// Write sends a problem response. retryAfter > 0 adds Retry-After (seconds).
func Write(w http.ResponseWriter, r *http.Request, status int, code, detail string, retryAfter int) {
	send(w, New(r, status, code, detail), retryAfter)
}

// WriteQuota sends 429 quota_exhausted with the quota member.
func WriteQuota(w http.ResponseWriter, r *http.Request, q Quota, retryAfter int) {
	p := New(r, http.StatusTooManyRequests, CodeQuotaExhausted, "")
	q.ResetsAt = q.ResetsAt.UTC()
	p.Quota = &q
	send(w, p, retryAfter)
}

func send(w http.ResponseWriter, p Problem, retryAfter int) {
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	if retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
