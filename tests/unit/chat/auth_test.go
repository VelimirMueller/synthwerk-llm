package chat_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/auth/authtest"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

// panicStore fails the test on any call: a refused request must not reach the DB.
type panicStore struct{ t *testing.T }

func (s panicStore) fail() { s.t.Error("the store was called"); panic("store called") }

func (s panicStore) CreateConversation(context.Context, store.Owner, string) (store.Conversation, error) {
	s.fail()
	return store.Conversation{}, nil
}

func (s panicStore) GetConversation(context.Context, store.Owner, string) (store.Conversation, error) {
	s.fail()
	return store.Conversation{}, nil
}

func (s panicStore) ConversationAlias(context.Context, store.Owner, string) (string, error) {
	s.fail()
	return "", nil
}

func (s panicStore) StartTurn(context.Context, store.Owner, string, string, store.Quota) (store.Turn, error) {
	s.fail()
	return store.Turn{}, nil
}

func (s panicStore) Finish(context.Context, store.Owner, store.Final, cloudevents.Event) error {
	s.fail()
	return nil
}

// panicProvider fails the test when the model is called.
type panicProvider struct{ t *testing.T }

func (panicProvider) Name() string { return "panic" }

func (p panicProvider) Chat(context.Context, provider.Request) (provider.Stream, error) {
	p.t.Error("the provider was called")
	panic("provider called")
}

const convPath = "/v1/conversations/0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e10/messages"

// Send - no valid token - 401, no store call, no model call (issue AC 2).
func TestSendWithoutValidTokenIsRefusedBeforeAnyModelCall(t *testing.T) {
	for _, withPanicProvider := range []bool{false, true} {
		var opts harness.Options
		if withPanicProvider {
			opts.Provider = panicProvider{t}
		}
		h := harness.New(t, panicStore{t}, opts)
		other := authtest.NewIssuer(t) // trusted by nobody
		cases := map[string]string{
			"no header":     "",
			"malformed":     "not.a.jwt",
			"expired":       h.Issuer.Mint(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "iat": time.Now().Add(-2 * time.Hour).Unix(), "nbf": time.Now().Add(-2 * time.Hour).Unix()}),
			"bad signature": h.Issuer.MintUnpublished(t, nil),
			"other issuer":  other.Mint(t, nil),
			"wrong aud":     h.Issuer.Mint(t, map[string]any{"aud": []string{"other-api"}}),
			"alg none":      h.Issuer.MintNone(t, nil),
			"alg HS256":     h.Issuer.MintHS256(t, nil),
			"no sub":        h.Issuer.Mint(t, map[string]any{"sub": nil}),
			"no org":        h.Issuer.Mint(t, map[string]any{"urn:zitadel:iam:user:resourceowner:id": nil}),
		}
		routes := []struct{ method, path string }{
			{http.MethodPost, convPath},
			{http.MethodPost, "/v1/conversations"},
			{http.MethodGet, "/v1/conversations/0199f3b0-1a2c-7c3d-8e4f-5a6b7c8d9e10"},
		}
		for name, tok := range cases {
			for _, rt := range routes {
				res := h.Do(t, rt.method, rt.path, tok, map[string]string{"content": "secret prompt"})
				if res.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s %s %s: status %d, want 401", name, rt.method, rt.path, res.StatusCode)
				}
				if res.Header.Get("WWW-Authenticate") == "" {
					t.Errorf("%s: no WWW-Authenticate header", name)
				}
				var p struct {
					Code string `json:"code"`
				}
				_ = json.NewDecoder(res.Body).Decode(&p)
				if p.Code != "unauthenticated" {
					t.Errorf("%s: code %q, want unauthenticated", name, p.Code)
				}
			}
		}
		if h.Fake != nil && h.Fake.Calls() != 0 {
			t.Errorf("fake provider calls = %d, want 0", h.Fake.Calls())
		}
	}
}

// Send - origin claim differs from the Origin header - 403 before the store.
func TestSendWithOtherOriginIsForbidden(t *testing.T) {
	h := harness.New(t, panicStore{t}, harness.Options{})
	tok := h.Issuer.Mint(t, map[string]any{"synthwerk:origin": "https://shop.example"})
	res := h.Do(t, http.MethodPost, convPath, tok, map[string]string{"content": "hi"}, "Origin", "https://evil.example")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", res.StatusCode)
	}
	if h.Fake.Calls() != 0 {
		t.Fatalf("fake provider calls = %d, want 0", h.Fake.Calls())
	}
}
