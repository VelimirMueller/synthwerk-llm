//go:build integration

package chat_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/VelimirMueller/synthwerk-contracts/go/llm"
)

func list(t *testing.T, r *rig, token, query string, header ...string) (int, llm.ConversationPage) {
	t.Helper()
	res := r.Do(t, http.MethodGet, "/v1/conversations"+query, token, nil, header...)
	var page llm.ConversationPage
	if res.StatusCode == http.StatusOK {
		dec := json.NewDecoder(res.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&page); err != nil {
			t.Fatal(err)
		}
	}
	return res.StatusCode, page
}

func ids(p llm.ConversationPage) []string {
	out := make([]string, 0, len(p.Items))
	for _, it := range p.Items {
		out = append(out, it.Id.String())
	}
	return out
}

// List - two users and the same sub in another org - each sees only the own
// conversations of the token org, newest first, paged; empty and deleted ones are
// left out; a second user cannot open the first user's conversation (synthwerk#54).
func TestListShowsOnlyTheOwnConversations(t *testing.T) {
	r := newRig(t, nil)
	tokA, tokB := r.Token(t, "user-a", "org-1"), r.Token(t, "user-b", "org-1")
	var mine []string
	for _, text := range []string{"first   question\nhere", "second question", "third question"} {
		id := r.Create(t, tokA)
		if res, _ := r.Send(t, tokA, id, text); res.StatusCode != http.StatusOK {
			t.Fatalf("send: %d", res.StatusCode)
		}
		mine = append(mine, id)
	}
	r.Create(t, tokA) // empty: not listed
	deleted := r.Create(t, tokA)
	if res, _ := r.Send(t, tokA, deleted, "gone"); res.StatusCode != http.StatusOK {
		t.Fatal("send")
	}
	if _, err := r.admin(t).Exec(t.Context(), `UPDATE llm.conversation SET deleted_at = now() WHERE id = $1`, deleted); err != nil {
		t.Fatal(err)
	}
	peer := r.Create(t, tokB)
	if res, _ := r.Send(t, tokB, peer, "peer secret"); res.StatusCode != http.StatusOK {
		t.Fatal("send")
	}
	otherOrg := r.Token(t, "user-a", "org-2")
	elsewhere := r.Create(t, otherOrg)
	if res, _ := r.Send(t, otherOrg, elsewhere, "other org"); res.StatusCode != http.StatusOK {
		t.Fatal("send")
	}

	status, page := list(t, r, tokA, "")
	want := []string{mine[2], mine[1], mine[0]}
	if status != http.StatusOK || !slices.Equal(ids(page), want) || page.NextCursor != nil {
		t.Fatalf("A: %d %v, want %v", status, ids(page), want)
	}
	if page.Items[2].Title != "first question here" || page.Items[0].Model != "default" {
		t.Fatalf("items %+v", page.Items)
	}

	// Paging: one per page walks back without a gap or a repeat.
	var walked []string
	query := "?limit=1"
	for range 4 {
		status, p := list(t, r, tokA, query)
		if status != http.StatusOK {
			t.Fatalf("page: %d", status)
		}
		walked = append(walked, ids(p)...)
		if p.NextCursor == nil {
			break
		}
		query = "?limit=1&cursor=" + *p.NextCursor
	}
	if !slices.Equal(walked, want) {
		t.Fatalf("walked %v, want %v", walked, want)
	}

	if _, p := list(t, r, tokB, ""); !slices.Equal(ids(p), []string{peer}) {
		t.Fatalf("B sees %v", ids(p))
	}
	if _, p := list(t, r, otherOrg, ""); !slices.Equal(ids(p), []string{elsewhere}) {
		t.Fatalf("same sub, org-2 sees %v", ids(p))
	}
	// Done looks like: a second user cannot open the first user's conversation.
	for _, id := range mine {
		if res := r.Do(t, http.MethodGet, "/v1/conversations/"+id, tokB, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("B opens A's %s: %d", id, res.StatusCode)
		}
	}
}

// List, Get, Send - widget token of origin A - never sees a conversation made on
// origin B or with a user token; the user token sees every origin (synthwerk#54).
func TestWidgetOriginBindsTheConversations(t *testing.T) {
	r := newRig(t, nil)
	const siteA, siteB = "https://a.example", "https://b.example"
	wA := r.WidgetToken(t, "user-a", "org-1", siteA)
	wB := r.WidgetToken(t, "user-a", "org-1", siteB)
	user := r.Token(t, "user-a", "org-1")

	onA := r.Create(t, wA, "Origin", siteA)
	if res, _ := r.Send(t, wA, onA, "asked on A", "Origin", siteA); res.StatusCode != http.StatusOK {
		t.Fatal("send A")
	}
	onB := r.Create(t, wB, "Origin", siteB)
	if res, _ := r.Send(t, wB, onB, "asked on B", "Origin", siteB); res.StatusCode != http.StatusOK {
		t.Fatal("send B")
	}
	first := r.Create(t, user)
	if res, _ := r.Send(t, user, first, "asked first-party"); res.StatusCode != http.StatusOK {
		t.Fatal("send user")
	}

	if _, p := list(t, r, wA, "", "Origin", siteA); !slices.Equal(ids(p), []string{onA}) {
		t.Fatalf("widget A lists %v, want only %s", ids(p), onA)
	}
	if _, p := list(t, r, wB, "", "Origin", siteB); !slices.Equal(ids(p), []string{onB}) {
		t.Fatalf("widget B lists %v, want only %s", ids(p), onB)
	}
	if _, p := list(t, r, user, ""); !slices.Equal(ids(p), []string{first, onB, onA}) {
		t.Fatalf("user token lists %v", ids(p))
	}
	calls := r.Fake.Calls()
	for _, id := range []string{onB, first} {
		if res := r.Do(t, http.MethodGet, "/v1/conversations/"+id, wA, nil, "Origin", siteA); res.StatusCode != http.StatusNotFound {
			t.Fatalf("widget A reads %s: %d", id, res.StatusCode)
		}
		if res, _ := r.Send(t, wA, id, "let me in", "Origin", siteA); res.StatusCode != http.StatusNotFound {
			t.Fatalf("widget A sends to %s: %d", id, res.StatusCode)
		}
	}
	if r.Fake.Calls() != calls {
		t.Fatal("a refused send reached the model")
	}
	if res := r.Do(t, http.MethodGet, "/v1/conversations/"+onA, user, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("user token reads the widget conversation: %d", res.StatusCode)
	}
}
