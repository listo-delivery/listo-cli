// Package login is the CLI's side of the sign-in host's device grant
// (RFC 8628) and of the refresh and revoke endpoints (RFC 6749, RFC 7009):
// the server's developer docs give the account of the same.
//
// Three things live here:
//
//   - Flow, the HTTP calls to the sign-in host (the device code, the polling of
//     the token endpoint, the refresh, the revoke). Every answer that is not a
//     2xx is an *OAuthError carrying the registered code ("authorization_pending",
//     "slow_down", "access_denied", "expired_token", "invalid_grant", ...).
//   - Poll, the loop that waits for the person to approve a code.
//   - GrantSource, the api.TokenSource of a stored grant: it answers the access
//     token while it has more than a minute left and otherwise refreshes under
//     config.Store.WithLock, re-reading the record after the lock is taken. A
//     refresh token is single use and one presented a second time ends the whole
//     grant on the server, so two terminals must never present the same one.
package login

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/version"
)

// ClientID is the first-party client the server holds for the CLI.
const ClientID = "listo-cli"

// DeviceGrantType is RFC 8628 section 3.4's grant_type.
const DeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// Scope asked for: reading and writing, because the CLI creates and cancels
// deliveries. A write is still behind --live and a prompt.
const Scope = "read write"

// DefaultInterval is RFC 8628's default polling interval when the server names none.
const DefaultInterval = 5 * time.Second

// SlowDownStep is what RFC 8628 section 3.5 says to add to the interval on slow_down.
const SlowDownStep = 5 * time.Second

// DeviceNameMax is the longest device name the server keeps.
const DeviceNameMax = 64

// OAuthError is an error answer of the sign-in host (RFC 6749 section 5.2).
type OAuthError struct {
	Status      int
	Code        string // "invalid_grant", "slow_down", ...; "" when the body was not an OAuth error
	Description string
	RetryAfter  time.Duration
}

func (e *OAuthError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("oauth: %d %s: %s", e.Status, e.Code, e.Description)
	}
	return fmt.Sprintf("oauth: status %d", e.Status)
}

// Is lets a caller write errors.Is(err, &login.OAuthError{Code: "invalid_grant"}).
func (e *OAuthError) Is(target error) bool {
	t, ok := target.(*OAuthError)
	return ok && t.Code != "" && t.Code == e.Code
}

// DeviceCode is the answer of POST /oauth/device/code (RFC 8628 section 3.2).
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// URL is the address to open: the complete one when the server sent it.
func (d DeviceCode) URL() string {
	if d.VerificationURIComplete != "" {
		return d.VerificationURIComplete
	}
	return d.VerificationURI
}

// Tokens is the answer of the token endpoint.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int       `json:"expires_in"`
	Scope        string    `json:"scope"`
	ExpiresAt    time.Time `json:"-"` // set by Flow from its clock when the answer arrives
}

// Flow talks to the sign-in host. The zero value of every optional field is the
// production behaviour.
type Flow struct {
	LoginURL string       // https://login.listo.delivery, no trailing slash
	HTTP     *http.Client // default: 30 second timeout
	Now      func() time.Time
	// Sleep waits d or returns ctx's error; tests replace it to run without waiting.
	Sleep func(ctx context.Context, d time.Duration) error
}

func (f *Flow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Flow) sleep(ctx context.Context, d time.Duration) error {
	if f.Sleep != nil {
		return f.Sleep(ctx, d)
	}
	return Sleep(ctx, d)
}

// Sleep waits d, or returns ctx's error when it is canceled first.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (f *Flow) client() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// post sends a form to the sign-in host and decodes a 2xx answer into out. Any
// other status is an *OAuthError; a failure to get an answer is an
// *api.TransportError.
func (f *Flow) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.LoginURL, "/")+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	req.Header.Set("Accept-Language", i18n.AcceptLanguage())
	resp, err := f.client().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &api.TransportError{Err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &api.TransportError{Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(body, out); err != nil {
			return &OAuthError{Status: resp.StatusCode, Code: "invalid_response", Description: "the answer was not JSON"}
		}
		return nil
	}
	oe := &OAuthError{Status: resp.StatusCode}
	var wire struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &wire) == nil {
		oe.Code, oe.Description = wire.Error, wire.Description
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		var n int
		if _, err := fmt.Sscanf(ra, "%d", &n); err == nil && n > 0 {
			oe.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return oe
}

// RequestDeviceCode starts a login: POST /oauth/device/code. requestedMode is
// "live" for `listo login --live` (it only preselects Live on the confirm page)
// and "" otherwise.
func (f *Flow) RequestDeviceCode(ctx context.Context, deviceName, requestedMode string) (*DeviceCode, error) {
	form := url.Values{"client_id": {ClientID}, "scope": {Scope}, "device_name": {CleanDeviceName(deviceName)}}
	if requestedMode == "live" {
		form.Set("requested_mode", "live")
	}
	var out DeviceCode
	if err := f.post(ctx, "/oauth/device/code", form, &out); err != nil {
		return nil, err
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, &OAuthError{Status: http.StatusOK, Code: "invalid_response", Description: "the answer had no device code"}
	}
	return &out, nil
}

// PollOnce is one POST /oauth/token with the device grant.
func (f *Flow) PollOnce(ctx context.Context, deviceCode string) (*Tokens, error) {
	return f.tokens(ctx, url.Values{"grant_type": {DeviceGrantType}, "client_id": {ClientID}, "device_code": {deviceCode}})
}

// Refresh exchanges a refresh token for a new pair. The old refresh token is
// dead from this moment: the caller must store the answer before anyone else
// reads the record (GrantSource does it under the config lock).
func (f *Flow) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	return f.tokens(ctx, url.Values{"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {refreshToken}})
}

func (f *Flow) tokens(ctx context.Context, form url.Values) (*Tokens, error) {
	var t Tokens
	if err := f.post(ctx, "/oauth/token", form, &t); err != nil {
		return nil, err
	}
	if t.AccessToken == "" {
		return nil, &OAuthError{Status: http.StatusOK, Code: "invalid_response", Description: "the answer had no access token"}
	}
	t.ExpiresAt = f.now().Add(time.Duration(t.ExpiresIn) * time.Second)
	return &t, nil
}

// Revoke ends the grant a token belongs to (RFC 7009). The server answers 200
// for any token, known or not.
func (f *Flow) Revoke(ctx context.Context, token string) error {
	return f.post(ctx, "/oauth/revoke", url.Values{"token": {token}, "client_id": {ClientID}}, nil)
}

// Reasons Poll gives up.
var (
	// ErrDenied is the person pressing "Rechazar" (access_denied).
	ErrDenied = errors.New("login: the person did not allow the connection")
	// ErrExpired is the code running out (expired_token), by the server's word or by our clock.
	ErrExpired = errors.New("login: the code expired")
)

// maxTransient is how many answers in a row may fail to arrive (or be a server
// error) before polling gives up: a computer asleep for a minute should not lose
// a half-finished login, but a host that is down should not hang it.
const maxTransient = 5

// Poll waits for the person to decide, polling the token endpoint every
// d.Interval seconds (default 5), adding SlowDownStep on slow_down, until the
// code's own expires_in has passed (default 600 seconds). It returns the tokens,
// ErrDenied, ErrExpired, ctx's error, or the OAuthError of any other refusal
// ("invalid_grant": the code was already used).
//
// expiresIn and interval are seconds; zero or negative means the default.
func (f *Flow) Poll(ctx context.Context, deviceCode string, expiresIn, interval int) (*Tokens, error) {
	wait := time.Duration(interval) * time.Second
	if interval <= 0 {
		wait = DefaultInterval
	}
	life := time.Duration(expiresIn) * time.Second
	if expiresIn <= 0 {
		life = 10 * time.Minute
	}
	deadline := f.now().Add(life)
	transient := 0
	for {
		if err := f.sleep(ctx, wait); err != nil {
			return nil, err
		}
		if !f.now().Before(deadline) {
			return nil, ErrExpired
		}
		tok, err := f.PollOnce(ctx, deviceCode)
		if err == nil {
			return tok, nil
		}
		var oe *OAuthError
		if errors.As(err, &oe) {
			switch {
			case oe.Code == "authorization_pending":
				transient = 0
				continue
			case oe.Code == "slow_down" || oe.Status == http.StatusTooManyRequests:
				transient = 0
				wait += SlowDownStep
				if oe.RetryAfter > wait {
					wait = oe.RetryAfter
				}
				continue
			case oe.Code == "access_denied":
				return nil, ErrDenied
			case oe.Code == "expired_token":
				return nil, ErrExpired
			case oe.Status >= 500:
				transient++
				if transient >= maxTransient {
					return nil, err
				}
				continue
			}
			return nil, err
		}
		var te *api.TransportError
		if errors.As(err, &te) {
			transient++
			if transient >= maxTransient {
				return nil, err
			}
			continue
		}
		return nil, err
	}
}

// DeviceName is what the computer calls itself and its operating system, readable
// and at most 64 characters: "ana-mbp (macOS)".
func DeviceName() string {
	host, _ := os.Hostname()
	return deviceName(host, runtime.GOOS)
}

func deviceName(host, goos string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".local")
	if host == "" {
		host = "computer"
	}
	osName := map[string]string{"darwin": "macOS", "windows": "Windows", "linux": "Linux"}[goos]
	if osName == "" {
		osName = goos
	}
	suffix := " (" + osName + ")"
	return CleanDeviceName(truncateRunes(host, DeviceNameMax-len([]rune(suffix))) + suffix)
}

// CleanDeviceName removes control characters and cuts the name to DeviceNameMax characters.
func CleanDeviceName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return truncateRunes(strings.TrimSpace(b.String()), DeviceNameMax)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
