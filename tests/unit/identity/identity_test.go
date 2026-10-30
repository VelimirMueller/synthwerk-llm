package identity_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
)

const entitlements = `{"org_id":"0199f3a0-5b6c-7d8e-9f0a-1b2c3d4e5f60","plan":"free","limits":{"llm_tokens_per_user_per_day":50000,"llm_tokens_per_org_per_day":500000,"apps_per_org":3},"version":1,"updated_at":"2026-10-29T10:00:00Z"}`

func serve(t *testing.T, status int, body string, seen *http.Request) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r.Clone(r.Context())
		}
		if status == http.StatusFound {
			w.Header().Set("Location", "https://elsewhere.example/")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestMyEntitlements(t *testing.T) {
	t.Run("MyEntitlements - identity answers 200 - org id and per-user limit, caller token forwarded", func(t *testing.T) {
		var seen http.Request
		c, err := identity.New(serve(t, http.StatusOK, entitlements, &seen) + "/")
		if err != nil {
			t.Fatal(err)
		}
		p, err := c.MyEntitlements(t.Context(), "tok-123")
		if err != nil {
			t.Fatal(err)
		}
		if p.OrgID != "0199f3a0-5b6c-7d8e-9f0a-1b2c3d4e5f60" || p.UserTokensPerDay != 50000 {
			t.Fatalf("got %+v", p)
		}
		if seen.Method != http.MethodGet || seen.URL.Path != "/v1/me/entitlements" || seen.Header.Get("Authorization") != "Bearer tok-123" {
			t.Fatalf("request %s %s", seen.Method, seen.URL.Path)
		}
	})
	for _, c := range []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"404 not an active member", http.StatusNotFound, `{"code":"not_found"}`, identity.ErrDenied},
		{"401 token refused", http.StatusUnauthorized, `{}`, identity.ErrDenied},
		{"403 forbidden", http.StatusForbidden, `{}`, identity.ErrDenied},
		{"500", http.StatusInternalServerError, `{}`, identity.ErrUnavailable},
		{"302 redirect", http.StatusFound, ``, identity.ErrUnavailable},
		{"200 with a broken body", http.StatusOK, `{"org_id":`, identity.ErrUnavailable},
		{"200 without an org id", http.StatusOK, `{"plan":"free"}`, identity.ErrUnavailable},
	} {
		t.Run("MyEntitlements - "+c.name+" - "+c.want.Error(), func(t *testing.T) {
			cl, err := identity.New(serve(t, c.status, c.body, nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cl.MyEntitlements(t.Context(), "tok"); !errors.Is(err, c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	t.Run("MyEntitlements - identity unreachable - ErrUnavailable", func(t *testing.T) {
		cl, err := identity.New("http://127.0.0.1:1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cl.MyEntitlements(t.Context(), "tok"); !errors.Is(err, identity.ErrUnavailable) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("New - bad base URLs - refused", func(t *testing.T) {
		for _, u := range []string{"", "identity:8080", "ftp://x", "http://user:pw@x", "http://x/?a=1", "http://x/#f"} {
			if _, err := identity.New(u); err == nil {
				t.Errorf("%q accepted", u)
			}
		}
	})
}
