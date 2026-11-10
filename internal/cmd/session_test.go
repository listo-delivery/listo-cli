package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/config"
)

func loggedIn(t *testing.T, mode string) *fakeHost {
	t.Helper()
	h := newFakeHost(t)
	stubSeams(t)
	h.mode = mode
	args := []string{"login"}
	if mode == "live" {
		args = append(args, "--live")
	}
	if r := h.run(t, true, "", args...); r.code != 0 {
		t.Fatalf("login: %+v", r)
	}
	return h
}

func TestLogoutRevokesAndForgetsTheCredentialAndTheWebhookSecret(t *testing.T) {
	h := loggedIn(t, "test")
	store, err := config.Open(config.Options{Dir: h.dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.WebhookSecret(t.Context(), config.Test); err != nil {
		t.Fatal(err)
	}

	r := h.run(t, true, "", "logout")
	if r.code != 0 || !strings.Contains(r.out, "Logged out of Test mode on this computer.") {
		t.Fatalf("%+v", r)
	}
	if len(h.revokeForms) != 1 || h.revokeForms[0].Get("token") != "lso_rt_1" || h.revokeForms[0].Get("client_id") != "listo-cli" {
		t.Errorf("revoke %v", h.revokeForms)
	}
	rec := storedRecord(t, h.dir, config.Test)
	if rec.Credential != nil || rec.WebhookSecret != "" {
		t.Errorf("still stored: %+v", rec)
	}
	if !h.grantRevoked {
		t.Errorf("the server did not end the grant")
	}

	// Logging out of nothing says so and is not an error.
	r = h.run(t, true, "", "logout")
	if r.code != 0 || !strings.Contains(r.out, "Nothing to log out of in Test mode.") || len(h.revokeForms) != 1 {
		t.Fatalf("%+v", r)
	}
	r = h.run(t, true, "", "--lang", "es", "logout", "--live")
	if r.code != 0 || !strings.Contains(r.out, "No había sesión que cerrar en modo real.") {
		t.Fatalf("%+v", r)
	}
}

func TestLogoutOnlyClearsTheSelectedMode(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	h.keys["lst_live_abc"] = "live"
	if r := h.run(t, true, "", "login"); r.code != 0 {
		t.Fatal(r)
	}
	if r := h.run(t, true, "", "login", "--live", "--key", "lst_live_abc"); r.code != 0 {
		t.Fatal(r)
	}
	r := h.run(t, true, "", "logout", "--live")
	if r.code != 0 || !strings.Contains(r.out, "Logged out of Live mode") || !strings.Contains(r.out, "The key itself still works.") {
		t.Fatalf("%+v", r)
	}
	// A key is only forgotten here: no server call.
	if len(h.revokeForms) != 0 {
		t.Errorf("a key was revoked through the sign-in host: %v", h.revokeForms)
	}
	if storedRecord(t, h.dir, config.Live).Credential != nil || storedRecord(t, h.dir, config.Test).Credential == nil {
		t.Errorf("wrong mode cleared")
	}
}

func TestLogoutStillLogsOutWhenTheServerCannotBeReached(t *testing.T) {
	h := loggedIn(t, "test")
	h.srv.Close()
	r := h.run(t, true, "", "logout")
	if r.code != 0 || !strings.Contains(r.out, "Could not reach the server to end the session there") || !strings.Contains(r.out, "Logged out of Test mode") {
		t.Fatalf("%+v", r)
	}
	if storedRecord(t, h.dir, config.Test).Credential != nil {
		t.Errorf("credential kept")
	}
}

func TestWhoamiTextAndJSON(t *testing.T) {
	h := loggedIn(t, "test")
	r := h.run(t, true, "", "whoami")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"Company: Acme Delivery (acme)", "Mode: [TEST]", "Credential: sign-in on \""} {
		if !strings.Contains(r.out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.out)
		}
	}
	r = h.run(t, false, "", "whoami")
	var j whoamiOut
	if err := json.Unmarshal([]byte(r.out), &j); err != nil || j.Partner.Name != "Acme Delivery" || j.Partner.Slug != "acme" || j.Mode != "test" || j.Credential != "sign_in" || j.Device == "" {
		t.Fatalf("%q %v", r.out, err)
	}
	r = h.run(t, true, "", "--lang", "es", "whoami")
	if !strings.Contains(r.out, "Empresa: Acme Delivery (acme)") || !strings.Contains(r.out, "Credencial: inicio de sesión en") {
		t.Errorf("%s", r.out)
	}

	// Live is a separate login.
	r = h.run(t, true, "", "whoami", "--live")
	if r.code != 1 || !strings.Contains(r.err, "No live session on this computer.") || !strings.Contains(r.err, "Next step: Run listo login --live.") {
		t.Errorf("%+v", r)
	}
}

func TestWhoamiWithAKeyAndWithTheEnvironmentKey(t *testing.T) {
	h := newFakeHost(t)
	h.keys["lst_test_abc"] = "test"
	h.keys["lst_live_env"] = "live"
	if r := h.run(t, true, "", "login", "--key", "lst_test_abc"); r.code != 0 {
		t.Fatal(r)
	}
	r := h.run(t, true, "", "whoami")
	if r.code != 0 || !strings.Contains(r.out, "Credential: API key\n") {
		t.Fatalf("%+v", r)
	}
	r = h.runEnv(t, true, "", map[string]string{"LISTO_API_KEY": "lst_live_env"}, "whoami", "--live")
	if r.code != 0 || !strings.Contains(r.out, "Mode: [LIVE]") || !strings.Contains(r.out, "Credential: API key from LISTO_API_KEY") {
		t.Fatalf("%+v", r)
	}
}

func TestACommandRefreshesAnExpiredAccessTokenAndStoresTheNewPair(t *testing.T) {
	h := loggedIn(t, "test")
	store, _ := config.Open(config.Options{Dir: h.dir})
	if _, err := store.Update(t.Context(), config.Test, func(r *config.Record) error {
		r.Credential.AccessTokenExpiresAt = time.Now().Add(30 * time.Second) // inside the 60 second margin
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := h.run(t, true, "", "whoami")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if h.refreshes != 1 {
		t.Errorf("refreshes %d", h.refreshes)
	}
	c := storedRecord(t, h.dir, config.Test).Credential
	if c.AccessToken != "lso_at_2" || c.RefreshToken != "lso_rt_2" || time.Until(c.AccessTokenExpiresAt) < 50*time.Minute || c.DeviceName == "" {
		t.Errorf("%+v", c)
	}
	if last := h.pingTokens[len(h.pingTokens)-1]; last != "lso_at_2" {
		t.Errorf("ping used %q", last)
	}
	// Fresh now: the next command refreshes nothing.
	if r := h.run(t, true, "", "whoami"); r.code != 0 || h.refreshes != 1 {
		t.Fatalf("%+v refreshes %d", r, h.refreshes)
	}
}

func TestARejectedTokenRefreshesOnceAndRetries(t *testing.T) {
	h := loggedIn(t, "test")
	h.mu.Lock()
	h.access = map[string]bool{} // the server forgot the access token (revoked, rotated by another computer)
	h.mu.Unlock()
	r := h.run(t, true, "", "whoami")
	if r.code != 0 || h.refreshes != 1 {
		t.Fatalf("%+v refreshes %d", r, h.refreshes)
	}
}

func TestSessionEndedDeletesTheGrantAndNamesTheNextStep(t *testing.T) {
	for _, tc := range []struct {
		mode, flag, next string
	}{
		{"test", "", "Next step: Run listo login.\n"},
		{"live", "--live", "Next step: Run listo login --live.\n"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			h := loggedIn(t, tc.mode)
			store, _ := config.Open(config.Options{Dir: h.dir})
			m := config.ModeFor(tc.mode == "live")
			if _, err := store.Update(t.Context(), m, func(r *config.Record) error {
				r.Credential.AccessTokenExpiresAt = time.Now().Add(-time.Minute)
				r.WebhookSecret = "whsec_keep"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			h.mu.Lock()
			h.failRefresh = true
			h.mu.Unlock()
			args := []string{"whoami"}
			if tc.flag != "" {
				args = append(args, tc.flag)
			}
			r := h.run(t, true, "", args...)
			if r.code != 1 || !strings.Contains(r.err, "Session ended.") || !strings.Contains(r.err, tc.next) {
				t.Fatalf("%+v", r)
			}
			rec := storedRecord(t, h.dir, m)
			if rec.Credential != nil {
				t.Errorf("the dead grant was kept: %+v", rec.Credential)
			}
			if rec.WebhookSecret != "whsec_keep" {
				t.Errorf("the webhook secret belongs to the computer, not the grant: %q", rec.WebhookSecret)
			}
			// The next command does not go to the network at all.
			before := h.refreshes
			r = h.run(t, true, "", args...)
			if r.code != 1 || !strings.Contains(r.err, "You are not logged in") && !strings.Contains(r.err, "No live session") || h.refreshes != before {
				t.Errorf("%+v", r)
			}
		})
	}
}

func TestTwoCommandsRacingARefreshPresentTheTokenOnce(t *testing.T) {
	h := loggedIn(t, "test")
	store, _ := config.Open(config.Options{Dir: h.dir})
	if _, err := store.Update(t.Context(), config.Test, func(r *config.Record) error {
		r.Credential.AccessTokenExpiresAt = time.Now().Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Execute keeps its state in package variables, so the processes' race is
	// proven at the source level (internal/login); here two sequential commands
	// stand for "the second terminal arrives after the first has refreshed".
	for i := 0; i < 2; i++ {
		if r := h.run(t, true, "", "whoami"); r.code != 0 {
			t.Fatalf("%+v", r)
		}
	}
	if h.refreshes != 1 || h.grantRevoked {
		t.Errorf("refreshes %d, revoked %v", h.refreshes, h.grantRevoked)
	}
}
