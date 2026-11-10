package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/login"
)

// fakeHost plays the API and the sign-in host at once (LISTO_API_URL and
// LISTO_LOGIN_URL both point at it). It follows the server's documented
// device-grant contract: opaque tokens, a refresh token that is
// rotated on use, and the whole grant revoked when a rotated-away one returns.
type fakeHost struct {
	t   *testing.T
	srv *httptest.Server
	dir string

	mu sync.Mutex

	// What ping answers for a bearer that belongs to the grant.
	mode string

	// What the next polls answer, in order; the last one repeats. "" is success.
	polls []string
	// firstPollWaits, when set, holds the first poll until it is closed (the
	// test's proof that Enter opened the browser before the answer came).
	firstPollWaits chan struct{}

	deviceForms []url.Values
	tokenForms  []url.Values
	revokeForms []url.Values
	pingTokens  []string

	// The grant.
	seq          int
	access       map[string]bool // live access tokens
	refresh      string          // the current refresh token
	retired      map[string]bool
	grantRevoked bool
	refreshes    int
	// failRefresh answers invalid_grant to the next refresh without revoking.
	failRefresh bool

	// keys the ping accepts.
	keys map[string]string // key -> mode
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{t: t, dir: t.TempDir(), mode: "test", access: map[string]bool{}, retired: map[string]bool{}, keys: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/device/code", h.deviceCode)
	mux.HandleFunc("POST /oauth/token", h.token)
	mux.HandleFunc("POST /oauth/revoke", h.revoke)
	mux.HandleFunc("GET /v1/ping", h.ping)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *fakeHost) env(extra map[string]string) map[string]string {
	env := map[string]string{
		"LISTO_API_URL":    h.srv.URL,
		"LISTO_LOGIN_URL":  h.srv.URL,
		"LISTO_CONFIG_DIR": h.dir,
		"LANG":             "en_US.UTF-8",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func (h *fakeHost) run(t *testing.T, tty bool, in string, args ...string) result {
	t.Helper()
	return run(t, tty, in, h.env(nil), args...)
}

func (h *fakeHost) runEnv(t *testing.T, tty bool, in string, extra map[string]string, args ...string) result {
	t.Helper()
	return run(t, tty, in, h.env(extra), args...)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": "fake " + code})
}

func (h *fakeHost) deviceCode(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h.mu.Lock()
	h.deviceForms = append(h.deviceForms, r.PostForm)
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"device_code":               "lso_dc_FAKE",
		"user_code":                 "ABCD-EFGH",
		"verification_uri":          h.srv.URL + "/cli/confirm",
		"verification_uri_complete": h.srv.URL + "/cli/confirm?code=ABCD-EFGH",
		"expires_in":                600,
		"interval":                  5,
	})
}

// issue mints a pair and makes it the grant's current one. Caller holds h.mu.
func (h *fakeHost) issue() map[string]any {
	h.seq++
	at := "lso_at_" + strconv.Itoa(h.seq)
	if h.refresh != "" {
		h.retired[h.refresh] = true
	}
	h.refresh = "lso_rt_" + strconv.Itoa(h.seq)
	h.access[at] = true
	return map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 3600, "refresh_token": h.refresh, "scope": "read write"}
}

func (h *fakeHost) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	form := r.PostForm
	h.mu.Lock()
	h.tokenForms = append(h.tokenForms, form)
	wait := h.firstPollWaits
	if form.Get("grant_type") == login.DeviceGrantType {
		h.firstPollWaits = nil
	} else {
		wait = nil
	}
	h.mu.Unlock()
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	switch form.Get("grant_type") {
	case login.DeviceGrantType:
		answer := ""
		if len(h.polls) > 0 {
			answer = h.polls[0]
			if len(h.polls) > 1 {
				h.polls = h.polls[1:]
			}
		}
		if answer != "" {
			oauthError(w, 400, answer)
			return
		}
		writeJSON(w, 200, h.issue())
	case "refresh_token":
		h.refreshes++
		presented := form.Get("refresh_token")
		switch {
		case h.failRefresh:
			h.failRefresh = false
			oauthError(w, 400, "invalid_grant")
		case h.grantRevoked || (presented != h.refresh && !h.retired[presented]):
			oauthError(w, 400, "invalid_grant")
		case h.retired[presented]:
			// A rotated-away token coming back: the server ends the grant.
			h.grantRevoked = true
			oauthError(w, 400, "invalid_grant")
		default:
			writeJSON(w, 200, h.issue())
		}
	default:
		oauthError(w, 400, "unsupported_grant_type")
	}
}

func (h *fakeHost) revoke(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h.mu.Lock()
	h.revokeForms = append(h.revokeForms, r.PostForm)
	if r.PostForm.Get("token") == h.refresh {
		h.grantRevoked = true
	}
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{})
}

func (h *fakeHost) ping(w http.ResponseWriter, r *http.Request) {
	bearer := r.Header.Get("Authorization")
	if len(bearer) > 7 {
		bearer = bearer[7:]
	}
	h.mu.Lock()
	h.pingTokens = append(h.pingTokens, bearer)
	mode := h.mode
	ok := h.access[bearer] && !h.grantRevoked
	if m, isKey := h.keys[bearer]; isKey {
		mode, ok = m, true
	}
	h.mu.Unlock()
	if !ok {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "invalid_api_key", "message": "Nope.", "request_id": "req-1", "doc_url": "https://docs.listo.delivery/en/errors/invalid_api_key"}})
		return
	}
	writeJSON(w, 200, map[string]any{"partner": map[string]string{"slug": "acme", "name": "Acme Delivery"}, "mode": mode})
}

func (h *fakeHost) count(f func() int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return f()
}
