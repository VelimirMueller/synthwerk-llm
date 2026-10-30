package chat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"

	"github.com/VelimirMueller/synthwerk-llm/internal/quota"
	"github.com/VelimirMueller/synthwerk-llm/internal/settings"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

const (
	shop     = "https://shop.example"
	widgetID = "0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e10"
)

// ownerStore holds one conversation in memory and records the owner of every call.
type ownerStore struct {
	panicStore
	mu     sync.Mutex
	owner  *store.Owner
	owners []store.Owner
	limits []quota.Limits
}

func (s *ownerStore) seen(o store.Owner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, o)
}

func (s *ownerStore) mine(o store.Owner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner != nil && *s.owner == o
}

func (s *ownerStore) CreateConversation(_ context.Context, o store.Owner, alias string) (store.Conversation, error) {
	s.seen(o)
	s.mu.Lock()
	s.owner = &o
	s.mu.Unlock()
	return store.Conversation{ID: widgetID, ModelAlias: alias, CreatedAt: time.Now()}, nil
}

func (s *ownerStore) GetConversation(_ context.Context, o store.Owner, _ string) (store.Conversation, error) {
	s.seen(o)
	if !s.mine(o) {
		return store.Conversation{}, store.ErrNotFound
	}
	return store.Conversation{ID: widgetID, ModelAlias: "default", CreatedAt: time.Now()}, nil
}

func (s *ownerStore) ConversationAlias(_ context.Context, o store.Owner, _ string) (string, error) {
	s.seen(o)
	if !s.mine(o) {
		return "", store.ErrNotFound
	}
	return "default", nil
}

func (s *ownerStore) StartTurn(_ context.Context, o store.Owner, _, _ string, q store.Quota) (store.Turn, error) {
	s.seen(o)
	s.mu.Lock()
	s.limits = append(s.limits, q.Limits)
	s.mu.Unlock()
	q.Tokens(nil)
	return store.Turn{UserMessageID: "0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e11", AssistantMessageID: "0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e12"}, nil
}

func (s *ownerStore) Finish(_ context.Context, o store.Owner, _ store.Final, _ cloudevents.Event) error {
	s.seen(o)
	return nil
}

func (s *ownerStore) OrgSettings(_ context.Context, o store.Owner) (*settings.Settings, error) {
	s.seen(o)
	return nil, nil
}

// Create, Get, Send - widget token from its origin - 201, 200, stream; every store call
// is owned by the token org and sub, and the quota limits apply (#30, #33).
func TestWidgetTokenChatsFromItsOrigin(t *testing.T) {
	st := &ownerStore{panicStore: panicStore{t}}
	h := harness.New(t, st, harness.Options{Quota: quota.Static{UserTokens: 50000, OrgTokens: 500000}})
	tok := h.WidgetToken(t, "user-w", "org-1", shop)

	id := h.Create(t, tok, "Origin", shop)
	if res := h.Do(t, http.MethodGet, "/v1/conversations/"+id, tok, nil, "Origin", shop); res.StatusCode != http.StatusOK {
		t.Fatalf("get: status %d", res.StatusCode)
	}
	res, evs := h.Send(t, tok, id, "hi", "Origin", shop)
	if res.StatusCode != http.StatusOK || harness.Text(t, evs) == "" {
		t.Fatalf("send: status %d, events %v", res.StatusCode, harness.Names(evs))
	}

	want := store.Owner{Tenant: "org-1", User: "user-w"}
	st.mu.Lock()
	owners, limits := st.owners, st.limits
	st.mu.Unlock()
	if len(owners) == 0 {
		t.Fatal("no store call")
	}
	for _, o := range owners {
		if o != want {
			t.Fatalf("store owner %+v, want %+v", o, want)
		}
	}
	if len(limits) != 1 || limits[0].UserTokens != 50000 || limits[0].OrgTokens != 500000 {
		t.Fatalf("quota limits %+v", limits)
	}

	// The same user with a user token owns the same conversation: one quota key per sub.
	if res := h.Do(t, http.MethodGet, "/v1/conversations/"+id, h.Token(t, "user-w", "org-1"), nil); res.StatusCode != http.StatusOK {
		t.Fatalf("user token of the same sub: status %d", res.StatusCode)
	}
}

// Create, Get, Send - widget token refused before any store call (#30).
func TestWidgetTokenRefusals(t *testing.T) {
	h := harness.New(t, panicStore{t}, harness.Options{Provider: panicProvider{t}})
	tok := h.WidgetToken(t, "user-w", "org-1", shop)
	noOrigin := h.WidgetToken(t, "user-w", "org-1", "", map[string]any{auth.ClaimOrigin: nil})
	emptyOrigin := h.WidgetToken(t, "user-w", "org-1", "")
	numOrigin := h.WidgetToken(t, "user-w", "org-1", "", map[string]any{auth.ClaimOrigin: 7})

	routes := []struct{ method, path string }{
		{http.MethodPost, "/v1/conversations"},
		{http.MethodGet, "/v1/conversations/" + widgetID},
		{http.MethodPost, "/v1/conversations/" + widgetID + "/messages"},
	}
	cases := []struct {
		name   string
		token  string
		origin string
		status int
	}{
		{"another origin - 403", tok, "https://evil.example", http.StatusForbidden},
		{"no Origin header - 403", tok, "", http.StatusForbidden},
		{"origin with a trailing slash - 403", tok, shop + "/", http.StatusForbidden},
		{"token without synthwerk:origin - 403", noOrigin, shop, http.StatusForbidden},
		{"token with an empty synthwerk:origin - 403", emptyOrigin, shop, http.StatusForbidden},
		{"token with a number as synthwerk:origin - 403", numOrigin, shop, http.StatusForbidden},
		{"widget token signed by the user issuer - 401", h.Issuer.MintType(t, "at+jwt", map[string]any{"iss": h.Widget.URL(), auth.ClaimOrigin: shop}), shop, http.StatusUnauthorized},
		{"widget token of another audience - 401", h.WidgetToken(t, "user-w", "org-1", shop, map[string]any{"aud": []string{"other-api"}}), shop, http.StatusUnauthorized},
		{"expired widget token - 401", h.WidgetToken(t, "user-w", "org-1", shop, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}), shop, http.StatusUnauthorized},
	}
	for _, rt := range routes {
		for _, tc := range cases {
			t.Run(rt.method+" "+rt.path+" - "+tc.name, func(t *testing.T) {
				var header []string
				if tc.origin != "" {
					header = []string{"Origin", tc.origin}
				}
				var body any
				if rt.method == http.MethodPost {
					body = map[string]string{"content": "hi"}
					if rt.path == "/v1/conversations" {
						body = map[string]any{}
					}
				}
				res := h.Do(t, rt.method, rt.path, tc.token, body, header...)
				if res.StatusCode != tc.status {
					t.Fatalf("status %d, want %d", res.StatusCode, tc.status)
				}
			})
		}
	}
}

// ExportMe - widget token from its origin - 403 forbidden, no store call: the export is
// for the user only (#41, #30).
func TestWidgetTokenCannotExport(t *testing.T) {
	h := harness.New(t, panicStore{t}, harness.Options{})
	for name, tok := range map[string]string{
		"widget token":                 h.WidgetToken(t, "user-w", "org-1", shop),
		"widget token with admin role": h.WidgetToken(t, "user-w", "org-1", shop, map[string]any{auth.ClaimRoles: map[string]any{auth.RoleAdmin: map[string]any{"org-1": "x"}}}),
	} {
		t.Run(name+" - 403", func(t *testing.T) {
			res := h.Do(t, http.MethodGet, "/v1/me/export", tok, nil, "Origin", shop)
			var p struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(res.Body).Decode(&p)
			if res.StatusCode != http.StatusForbidden || p.Code != "forbidden" {
				t.Fatalf("status %d code %q, want 403 forbidden", res.StatusCode, p.Code)
			}
		})
	}
}

// Create - widget issuer not configured - widget token 401, user token still works.
func TestWidgetTokenWithoutWidgetIssuer(t *testing.T) {
	on := harness.New(t, panicStore{t}, harness.Options{})
	tok := on.WidgetToken(t, "user-w", "org-1", shop)
	off := harness.New(t, panicStore{t}, harness.Options{NoWidget: true})
	if res := off.Do(t, http.MethodPost, "/v1/conversations", tok, map[string]any{}, "Origin", shop); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", res.StatusCode)
	}
}
