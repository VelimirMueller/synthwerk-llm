// Package tools holds the read-only AI tools of the chat (synthwerk#53). The model
// may ask for a tool; the gateway runs it with the scope of the verified token.
//
// Rules:
//   - Every tool is read-only and takes no arguments. Who the data belongs to comes
//     from the token (Scope), never from the model. A prompt cannot widen access:
//     there is no argument to put another user or org into.
//   - Arguments other than an empty object are refused (outcome denied). The model
//     output is untrusted.
//   - User tools read only the caller's own rows in the token org (row level
//     security, the same Owner as the chat). Admin tools need a user token with the
//     org role owner or admin and an active membership in identity.
//   - Each admin tool call of an active org member, allowed or refused, is an audit
//     fact (dev.synthwerk.llm.toolcall.executed.v3) that identity lists for the org.
//     Without an identity org id (widget token, no membership, identity down) there
//     is no org to list it under: the call is refused and only logged.
//   - Results are small JSON objects. They never hold another user's data.
//   - Logs hold the tool name and the outcome. Never arguments, never results.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
)

// Tool names (<service>_<verb>_<noun>, contracts docs/naming.md).
const (
	ListConversations = "llm_list_conversations"
	GetMyQuota        = "llm_get_my_quota"
	GetOrgUsage       = "llm_get_org_usage"
)

// Audit fact of an admin tool call (synthwerk-contracts events/catalog.yaml).
const (
	EventType  = "dev.synthwerk.llm.toolcall.executed.v3"
	dataSchema = "https://schemas.synthwerk.dev/events/llm/toolcall.executed.v3.json"
)

// Outcomes of a call (event data.outcome).
const (
	OutcomeOK     = "ok"
	OutcomeDenied = "denied"
	OutcomeError  = "error"
)

// listLimit is the number of conversations llm_list_conversations returns.
const listLimit = 20

// noArgs is the JSON Schema of every tool: an empty object.
var noArgs = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// Scope is who calls, from the verified token only.
type Scope struct {
	// Owner is the token org and sub, with the widget origin (#54).
	Owner store.Owner
	// Admin: a user token (no widget origin) with the org role owner or admin.
	Admin bool
	// Bearer is the caller's token, for the identity membership check of admin tools.
	Bearer string
	// ConversationID is the conversation the call runs in.
	ConversationID string
	// Limits are the quota limits of this send (plan and org cap applied).
	Limits quota.Limits
}

// Store is the data the tools read. *store.Store implements it.
type Store interface {
	ListConversations(ctx context.Context, o store.Owner, before string, limit int) ([]store.Summary, bool, error)
	QuotaUsage(ctx context.Context, o store.Owner, now time.Time) (store.Usage, error)
	RecordToolCall(ctx context.Context, o store.Owner, ev cloudevents.Event) error
}

// Members checks the membership in identity and gives the identity org id.
// *identity.Client implements it.
type Members interface {
	MyEntitlements(ctx context.Context, bearer string) (identity.Plan, error)
}

// Def is one tool as the model sees it.
type Def struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	// Admin tools are offered to Scope.Admin only.
	Admin bool
}

// Defs are the tools, in a fixed order. Keep the set small (#53).
var Defs = []Def{
	{Name: ListConversations, Description: "List the titles and dates of the caller's own last 20 conversations in this org. No arguments.", Parameters: noArgs},
	{Name: GetMyQuota, Description: "Show the caller's own token use and limit for today (UTC). No arguments.", Parameters: noArgs},
	{Name: GetOrgUsage, Description: "Admin only: show the token use and limit of the whole org for today (UTC). No arguments.", Parameters: noArgs, Admin: true},
}

// Registry runs the tools.
type Registry struct {
	store   Store
	members Members
	source  string
	log     *slog.Logger
	now     func() time.Time
}

// New returns the registry. members nil: identity is not configured, admin tools
// answer error. source is the CloudEvents source, /synthwerk/llm/<env>.
func New(st Store, members Members, source string, logger *slog.Logger) *Registry {
	return &Registry{store: st, members: members, source: source, log: logger, now: time.Now}
}

// For returns the tools the scope may call.
func (*Registry) For(s Scope) []Def {
	out := make([]Def, 0, len(Defs))
	for _, d := range Defs {
		if !d.Admin || s.Admin {
			out = append(out, d)
		}
	}
	return out
}

// result is the JSON text of a tool answer for the model.
func result(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `{"error":"internal"}`
	}
	return string(raw)
}

func refusal(reason string) string { return result(map[string]string{"error": reason}) }

// Call runs one tool call of the model and returns the JSON text for the model.
// It never returns an error: a refusal or a failure is a JSON error object, so the
// model can say what happened. name and args are model output (untrusted).
func (r *Registry) Call(ctx context.Context, s Scope, name, args string) string {
	start := time.Now()
	def, known := lookup(name)
	if !known {
		r.logCall(ctx, "unknown", OutcomeDenied, start)
		return refusal("unknown tool")
	}
	if def.Admin {
		return r.callAdmin(ctx, s, def, args, start)
	}
	if !emptyArgs(args) {
		r.logCall(ctx, def.Name, OutcomeDenied, start)
		return refusal("this tool takes no arguments; it always reads the caller's own data")
	}
	out, err := r.run(ctx, s, def.Name)
	if err != nil {
		r.logCall(ctx, def.Name, OutcomeError, start)
		return refusal("the tool failed")
	}
	r.logCall(ctx, def.Name, OutcomeOK, start)
	return out
}

// callAdmin runs an admin tool: token role, then the identity membership, then the
// tool. Every call with a known identity org becomes an audit fact.
func (r *Registry) callAdmin(ctx context.Context, s Scope, def Def, args string, start time.Time) string {
	if s.Owner.Origin != "" {
		// A widget token is never an admin token (#30): no identity call, no fact.
		r.logCall(ctx, def.Name, OutcomeDenied, start)
		return refusal("this tool is for org owners and admins only")
	}
	orgID, member := r.membership(ctx, s)
	outcome, out := OutcomeOK, ""
	switch {
	case !member:
		// Identity is down or not configured: no org id, so no audit fact.
		r.logCall(ctx, def.Name, OutcomeError, start)
		return refusal("the tool is not available now")
	case orgID == "" || !s.Admin:
		outcome, out = OutcomeDenied, refusal("this tool is for org owners and admins only")
	case !emptyArgs(args):
		outcome, out = OutcomeDenied, refusal("this tool takes no arguments; it always reads the caller's own org")
	default:
		var err error
		if out, err = r.run(ctx, s, def.Name); err != nil {
			outcome, out = OutcomeError, refusal("the tool failed")
		}
	}
	r.logCall(ctx, def.Name, outcome, start)
	if orgID != "" {
		r.audit(ctx, s, def.Name, orgID, outcome, time.Since(start))
	}
	return out
}

// membership asks identity. member false: identity did not answer usefully.
// orgID "": identity refused the caller (no active membership).
func (r *Registry) membership(ctx context.Context, s Scope) (orgID string, member bool) {
	if r.members == nil || s.Bearer == "" {
		return "", false
	}
	pl, err := r.members.MyEntitlements(ctx, s.Bearer)
	switch {
	case errors.Is(err, identity.ErrDenied):
		return "", true
	case err != nil:
		return "", false
	}
	return pl.OrgID, true
}

func (r *Registry) audit(ctx context.Context, s Scope, tool, orgID, outcome string, took time.Duration) {
	data := map[string]any{
		"tool":            tool,
		"scope":           "org",
		"org_id":          orgID,
		"conversation_id": s.ConversationID,
		"outcome":         outcome,
		"duration_ms":     min(took.Milliseconds(), 600000),
	}
	ctx = context.WithoutCancel(ctx)
	ev, err := cloudevents.New(ctx, EventType, r.source, data,
		cloudevents.WithSubject(s.ConversationID),
		cloudevents.WithOrg(orgID),
		cloudevents.WithActor("user:"+s.Owner.User),
		cloudevents.WithDataSchema(dataSchema))
	if err == nil {
		err = r.store.RecordToolCall(ctx, s.Owner, ev)
	}
	if err != nil {
		r.log.ErrorContext(ctx, "tool call not audited",
			slog.String("event.name", "llm.tool.audit_failed"),
			slog.String("gen_ai.tool.name", tool),
			slog.String("exception.type", fmt.Sprintf("%T", err)))
	}
}

func (r *Registry) run(ctx context.Context, s Scope, name string) (string, error) {
	switch name {
	case ListConversations:
		items, _, err := r.store.ListConversations(ctx, s.Owner, "", listLimit)
		if err != nil {
			return "", err
		}
		type item struct {
			Title     string `json:"title"`
			CreatedAt string `json:"created_at"`
		}
		list := make([]item, 0, len(items))
		for _, it := range items {
			list = append(list, item{Title: it.Title, CreatedAt: it.CreatedAt.UTC().Format(time.RFC3339)})
		}
		return result(map[string]any{"conversations": list}), nil
	case GetMyQuota:
		u, err := r.store.QuotaUsage(ctx, s.Owner, r.now())
		if err != nil {
			return "", err
		}
		return result(usage(u.Window, u.User, s.Limits.UserTokens)), nil
	case GetOrgUsage:
		u, err := r.store.QuotaUsage(ctx, s.Owner, r.now())
		if err != nil {
			return "", err
		}
		return result(usage(u.Window, u.Org, s.Limits.OrgTokens)), nil
	}
	return "", errors.New("tools: no such tool")
}

func usage(win time.Time, used, limit int64) map[string]any {
	return map[string]any{
		"window":       win.Format(time.DateOnly),
		"used_tokens":  used,
		"limit_tokens": limit, // 0: no limit
		"resets_at":    win.AddDate(0, 0, 1).Format(time.RFC3339),
	}
}

func (r *Registry) logCall(ctx context.Context, tool, outcome string, start time.Time) {
	r.log.LogAttrs(ctx, slog.LevelInfo, "tool call",
		slog.String("event.name", "llm.tool.called"),
		slog.String("gen_ai.tool.name", tool),
		slog.String("synthwerk.outcome", outcome),
		slog.Int64("synthwerk.latency_ms", time.Since(start).Milliseconds()))
}

func lookup(name string) (Def, bool) {
	for _, d := range Defs {
		if d.Name == name {
			return d, true
		}
	}
	return Def{}, false
}

// emptyArgs accepts "", "null" and an empty JSON object only.
func emptyArgs(args string) bool {
	a := strings.TrimSpace(args)
	if a == "" || a == "null" {
		return true
	}
	var m map[string]json.RawMessage
	return json.Unmarshal([]byte(a), &m) == nil && len(m) == 0
}
