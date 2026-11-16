package chat_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/VelimirMueller/synthwerk-llm/internal/store"
	"github.com/VelimirMueller/synthwerk-llm/tests/support/harness"
)

// Title - message shapes - white space joined, at most 80 characters, never empty (#54).
func TestTitle(t *testing.T) {
	long := strings.Repeat("ab ", 60)
	cases := map[string]struct{ in, want string }{
		"short":          {"How do I reset it?", "How do I reset it?"},
		"white space":    {"  line one\n\tline   two  ", "line one line two"},
		"only spaces":    {" \n\t ", "…"},
		"invalid utf-8":  {"ok \xff end", "ok � end"},
		"exactly 80":     {strings.Repeat("x", 80), strings.Repeat("x", 80)},
		"long, cut word": {long, strings.TrimRight(strings.Join(strings.Fields(long), " ")[:79], " ") + "…"},
	}
	for name, c := range cases {
		t.Run("Title - "+name, func(t *testing.T) {
			got := store.Title(c.in)
			if got != c.want || utf8.RuneCountInString(got) > store.TitleMax || got == "" {
				t.Fatalf("Title(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// List - bad paging or a widget token from another origin - refused before any store call (#54).
func TestListRefusedBeforeTheStore(t *testing.T) {
	h := harness.New(t, panicStore{t}, harness.Options{Provider: panicProvider{t}})
	tok := h.Token(t, "user-a", "org-1")
	cursor := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, query := range map[string]string{
		"limit 0":              "?limit=0",
		"limit 51":             "?limit=51",
		"limit not a number":   "?limit=ten",
		"limit with a plus":    "?limit=+5",
		"limit twice":          "?limit=5&limit=6",
		"cursor empty":         "?cursor=",
		"cursor not base64url": "?cursor=***",
		"cursor not a uuid":    "?cursor=" + cursor("not-a-uuid"),
		"cursor upper case":    "?cursor=" + cursor("0199F3B0-1A2C-7C3D-8E4F-5A6B7C8D9E10"),
		"cursor too long":      "?cursor=" + strings.Repeat("A", 65),
	} {
		t.Run("List - "+name+" - 400 invalid_request", func(t *testing.T) {
			res := h.Do(t, http.MethodGet, "/v1/conversations"+query, tok, nil)
			var p struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(res.Body).Decode(&p)
			if res.StatusCode != http.StatusBadRequest || p.Code != "invalid_request" {
				t.Fatalf("status %d code %q", res.StatusCode, p.Code)
			}
		})
	}
	t.Run("List - no token - 401", func(t *testing.T) {
		if res := h.Do(t, http.MethodGet, "/v1/conversations", "", nil); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status %d", res.StatusCode)
		}
	})
	t.Run("List - widget token, Origin header of another site - 403", func(t *testing.T) {
		w := h.WidgetToken(t, "user-a", "org-1", "https://a.example")
		if res := h.Do(t, http.MethodGet, "/v1/conversations", w, nil, "Origin", "https://b.example"); res.StatusCode != http.StatusForbidden {
			t.Fatalf("status %d", res.StatusCode)
		}
	})
}
