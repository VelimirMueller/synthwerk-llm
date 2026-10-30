package identity_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/VelimirMueller/synthwerk-llm/internal/identity"
)

func TestWidgetOriginListed(t *testing.T) {
	t.Run("WidgetOriginListed - identity answers 204 - yes; path, escaped origin, no token", func(t *testing.T) {
		var seen http.Request
		c, err := identity.New(serve(t, http.StatusNoContent, "", &seen))
		if err != nil {
			t.Fatal(err)
		}
		ok, err := c.WidgetOriginListed(t.Context(), "http://[::1]:3000")
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		if seen.URL.Path != "/internal/v1/widget-origins" || seen.URL.Query().Get("origin") != "http://[::1]:3000" || seen.Header.Get("Authorization") != "" {
			t.Fatalf("request %s %v", seen.URL, seen.Header)
		}
	})
	t.Run("WidgetOriginListed - identity answers 404 - no, no error", func(t *testing.T) {
		c, _ := identity.New(serve(t, http.StatusNotFound, `{"code":"not_found"}`, nil))
		if ok, err := c.WidgetOriginListed(t.Context(), "https://evil.test"); err != nil || ok {
			t.Fatalf("%v %v", ok, err)
		}
	})
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run("WidgetOriginListed - identity answers "+http.StatusText(status)+" - ErrUnavailable", func(t *testing.T) {
			c, _ := identity.New(serve(t, status, "", nil))
			if ok, err := c.WidgetOriginListed(t.Context(), "https://shop.example"); !errors.Is(err, identity.ErrUnavailable) || ok {
				t.Fatalf("%v %v", ok, err)
			}
		})
	}
	t.Run("WidgetOriginListed - identity unreachable - ErrUnavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		c, _ := identity.New(url)
		if ok, err := c.WidgetOriginListed(t.Context(), "https://shop.example"); !errors.Is(err, identity.ErrUnavailable) || ok {
			t.Fatalf("%v %v", ok, err)
		}
	})
}
