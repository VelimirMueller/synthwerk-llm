// Package identity asks synthwerk-identity about the caller's org (synthwerk#39)
// and about the widget origins for CORS (infra#21).
//
// Org (synthwerk#39):
// GET /v1/me/entitlements with the caller's own token. One call answers two
// questions for the AI settings: the stored membership is active (identity answers
// 404 otherwise), and the plan's per-user token limit (entitlements, #31). It also
// gives the identity org id, which the settings event carries.
package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	contract "github.com/VelimirMueller/synthwerk-contracts/go/identity"
)

// ErrDenied means identity refused the caller: no active membership in the token's
// org, or the token is not valid there. The handler answers 403.
var ErrDenied = errors.New("identity: caller refused")

// ErrUnavailable means identity did not answer usefully. The handler answers 503.
var ErrUnavailable = errors.New("identity: unavailable")

// maxBody bounds the answer of identity.
const maxBody = 64 << 10

// Plan is the part of the entitlements the AI settings need.
type Plan struct {
	// OrgID is the identity org id (UUID).
	OrgID string
	// UserTokensPerDay is the plan's per-user token limit per UTC day. 0: no limit.
	UserTokensPerDay int64
}

// Client calls identity.
type Client struct {
	base string
	http *http.Client
}

// New returns a client for the identity base URL (absolute http(s), no query).
func New(baseURL string) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("identity: base URL must be an absolute http(s) URL without user info, query or fragment")
	}
	return &Client{
		base: strings.TrimRight(u.String(), "/"),
		http: &http.Client{
			Timeout: 5 * time.Second,
			// Never follow a redirect with the caller's token.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// MyEntitlements returns the plan of the org the bearer token acts in.
func (c *Client) MyEntitlements(ctx context.Context, bearer string) (Plan, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/me/entitlements", nil)
	if err != nil {
		return Plan{}, fmt.Errorf("%w: request", ErrUnavailable)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		// The error text holds the URL only, never the token; keep the class.
		return Plan{}, fmt.Errorf("%w: no answer", ErrUnavailable)
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode == http.StatusUnauthorized, res.StatusCode == http.StatusForbidden, res.StatusCode == http.StatusNotFound:
		return Plan{}, ErrDenied
	case res.StatusCode != http.StatusOK:
		return Plan{}, fmt.Errorf("%w: status %d", ErrUnavailable, res.StatusCode)
	}
	var e contract.Entitlements
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&e); err != nil {
		return Plan{}, fmt.Errorf("%w: body", ErrUnavailable)
	}
	if e.OrgId.String() == "00000000-0000-0000-0000-000000000000" || e.Limits.LlmTokensPerUserPerDay < 0 {
		return Plan{}, fmt.Errorf("%w: body", ErrUnavailable)
	}
	return Plan{OrgID: e.OrgId.String(), UserTokensPerDay: e.Limits.LlmTokensPerUserPerDay}, nil
}

// widgetOriginsPath is the internal identity route that answers whether an app of
// an active org lists an origin (infra#21).
const widgetOriginsPath = "/internal/v1/widget-origins"

// WidgetOriginListed asks identity whether an app of an active org lists origin (a
// canonical origin). 204 is yes, 404 is no, anything else is ErrUnavailable. The
// call sends no token.
func (c *Client) WidgetOriginListed(ctx context.Context, origin string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+widgetOriginsPath+"?origin="+url.QueryEscape(origin), nil)
	if err != nil {
		return false, fmt.Errorf("%w: request", ErrUnavailable)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("%w: no answer", ErrUnavailable)
	}
	defer func() { _ = res.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, maxBody))
	switch res.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("%w: status %d", ErrUnavailable, res.StatusCode)
	}
}
