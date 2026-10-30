// Package harness runs the real llm HTTP stack in a test: the app with the token
// check (fake issuer from kit/auth/authtest), the chat handler, a fake provider that
// counts its calls, and a log buffer the tests search for leaked text.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth/authtest"
	kitlog "github.com/VelimirMueller/synthwerk-contracts/go/kit/log"

	"github.com/VelimirMueller/synthwerk-llm/internal/aisettings"
	"github.com/VelimirMueller/synthwerk-llm/internal/app"
	"github.com/VelimirMueller/synthwerk-llm/internal/chat"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/provider/fake"
	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/sse"
)

// Buffer is a goroutine-safe log sink.
type Buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// String returns everything logged so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// NopDB answers the readiness ping and the migrations without a database.
type NopDB struct{}

// Ping implements app.DB.
func (NopDB) Ping(context.Context) error { return nil }

// Exec implements app.DB.
func (NopDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// Options change the defaults of New.
type Options struct {
	// DB for the probes and migrations. Nil: NopDB.
	DB app.DB
	// Provider. Nil: a fake with no delay (H.Fake).
	Provider provider.Provider
	// Chat overrides fields of the default chat config when set.
	Chat func(*chat.Config)
	// Quota is the limit source. Nil: quota.None.
	Quota quota.Source
	// Plans answers the identity check of /v1/ai-settings (#39). Nil: identity is
	// not configured, the settings routes answer 503.
	Plans aisettings.Plans
	// NoWidget turns the widget issuer off: llm trusts the user issuer only.
	NoWidget bool
}

// H is a running gateway.
type H struct {
	Issuer *authtest.Issuer
	// Widget is the widget token issuer (identity, #30). Nil with Options.NoWidget.
	Widget *authtest.Issuer
	Fake   *fake.Provider
	Server *httptest.Server
	Logs   *Buffer
	App    *app.App
}

// New starts the gateway on st.
func New(t *testing.T, st chat.Store, o Options) *H {
	t.Helper()
	h := &H{Issuer: authtest.NewIssuer(t), Logs: &Buffer{}}
	logger, err := kitlog.New(kitlog.Options{ServiceName: "synthwerk-llm", Version: "0.0.0-test", Env: "ci", Level: -4, Format: "json", Writer: h.Logs})
	if err != nil {
		t.Fatal(err)
	}
	v, err := auth.NewVerifier(h.Issuer.Config())
	if err != nil {
		t.Fatal(err)
	}
	p := o.Provider
	if p == nil {
		h.Fake = fake.New(0)
		p = h.Fake
	} else if f, ok := p.(*fake.Provider); ok {
		h.Fake = f
	}
	cfg := chat.Config{
		Aliases:             map[string]string{"default": "test-model", "small": "test-small"},
		DefaultAlias:        "default",
		SystemPrompt:        "You are a test.",
		MaxInputChars:       8000,
		MaxStreamsPerOwner:  2,
		MaxContextChars:     24000,
		StreamTimeout:       30 * time.Second,
		ProviderConcurrency: 4,
		QueueTimeout:        2 * time.Second,
		Source:              "/synthwerk/llm/local",
	}
	if o.Chat != nil {
		o.Chat(&cfg)
	}
	db := o.DB
	if db == nil {
		db = NopDB{}
	}
	ch := chat.New(cfg, st, p, o.Quota, logger)
	deps := app.Deps{DB: db, Log: logger, Chat: ch, Verifier: v}
	if !o.NoWidget {
		// Identity signs widget tokens with ES256 and typ at+jwt.
		h.Widget = authtest.NewIssuer(t)
		h.Widget.Rotate(t, jose.ES256)
		if deps.WidgetVerifier, err = auth.NewVerifier(h.Widget.Config()); err != nil {
			t.Fatal(err)
		}
	}
	if ss, ok := st.(aisettings.Store); ok {
		deps.Settings = aisettings.New(ch.Service(), ss, o.Plans, cfg.Source, logger)
	}
	h.App, err = app.New(deps, app.Config{Version: "0.0.0", Commit: "0000000", Env: "ci"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.App.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	h.App.Health.MarkStarted()
	h.Server = httptest.NewServer(h.App.Handler())
	t.Cleanup(h.Server.Close)
	return h
}

// Token mints a user token for sub in org.
func (h *H) Token(t *testing.T, sub, org string) string {
	t.Helper()
	return h.Issuer.Mint(t, authtest.Claims(sub, org, auth.RoleMember))
}

// WidgetApp is the client_id (app id) of minted widget tokens.
const WidgetApp = "0199f3a0-0000-7000-8000-00000000a991"

// WidgetToken mints a widget token like identity does (#30): sub, org, client_id =
// app id, synthwerk:origin, no roles, typ at+jwt. extra overrides claims; a nil value
// removes one.
func (h *H) WidgetToken(t *testing.T, sub, org, origin string, extra ...map[string]any) string {
	t.Helper()
	if h.Widget == nil {
		t.Fatal("harness: the widget issuer is off")
	}
	c := map[string]any{
		"sub": sub, auth.ClaimOrgID: org, auth.ClaimClientID: WidgetApp,
		auth.ClaimOrigin: origin, "jti": "0199f3a0-0000-7000-8000-000000000001",
	}
	for _, e := range extra {
		maps.Copy(c, e)
	}
	return h.Widget.MintType(t, "at+jwt", c)
}

// TokenAs mints a user token for sub in org with one org role.
func (h *H) TokenAs(t *testing.T, sub, org, role string) string {
	t.Helper()
	return h.Issuer.Mint(t, authtest.Claims(sub, org, role))
}

// Do sends a request bound to the test. token "" sends none. body nil sends none.
func (h *H) Do(t *testing.T, method, path, token string, body any, header ...string) *http.Response {
	t.Helper()
	return h.DoCtx(t.Context(), t, method, path, token, body, header...)
}

// DoCtx is Do with its own context, for example to abort a stream.
func (h *H) DoCtx(ctx context.Context, t *testing.T, method, path, token string, body any, header ...string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.Server.URL+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := h.Server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// Create makes a conversation and returns its id. header: name, value pairs.
func (h *H) Create(t *testing.T, token string, header ...string) string {
	t.Helper()
	res := h.Do(t, http.MethodPost, "/v1/conversations", token, map[string]any{}, header...)
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d", res.StatusCode)
	}
	var c struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(res.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// Send posts a message and reads the whole answer stream. header: name, value pairs.
func (h *H) Send(t *testing.T, token, convID, content string, header ...string) (*http.Response, []sse.Event) {
	t.Helper()
	res := h.Do(t, http.MethodPost, "/v1/conversations/"+convID+"/messages", token, map[string]string{"content": content}, header...)
	if res.StatusCode != http.StatusOK {
		return res, nil
	}
	return res, sse.All(res.Body)
}

// Text joins the delta texts of a stream.
func Text(t *testing.T, events []sse.Event) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range events {
		if ev.Event != "delta" {
			continue
		}
		var d struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &d); err != nil {
			t.Fatal(err)
		}
		b.WriteString(d.Text)
	}
	return b.String()
}

// Names returns the event names, comments left out.
func Names(events []sse.Event) []string {
	var out []string
	for _, ev := range events {
		if ev.Event != "comment" {
			out = append(out, ev.Event)
		}
	}
	return out
}
