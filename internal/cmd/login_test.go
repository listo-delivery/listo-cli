package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/listo-delivery/listo-cli/internal/config"
)

// stubSeams makes polling instant and records what the browser was asked to open.
func stubSeams(t *testing.T) (opened *[]string, openedCh chan struct{}) {
	t.Helper()
	var (
		mu    sync.Mutex
		urls  []string
		once  sync.Once
		ch    = make(chan struct{})
		prevO = openBrowser
		prevS = pollSleep
	)
	openBrowser = func(u string) error {
		mu.Lock()
		urls = append(urls, u)
		mu.Unlock()
		once.Do(func() { close(ch) })
		return nil
	}
	pollSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { openBrowser, pollSleep = prevO, prevS })
	return &urls, ch
}

func storedRecord(t *testing.T, dir string, mode config.Mode) config.Record {
	t.Helper()
	s, err := config.Open(config.Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Load(mode)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestLoginInteractiveStoresTheGrantAndSaysWhere(t *testing.T) {
	h := newFakeHost(t)
	opened, openedCh := stubSeams(t)
	h.polls = []string{"authorization_pending", "authorization_pending", ""}
	h.firstPollWaits = openedCh // the answer comes only after Enter opened the browser

	r := h.run(t, true, "\n", "login")
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", r.code, r.out, r.err)
	}
	for _, want := range []string{
		"Your code is ABCD-EFGH",
		"Press Enter to open " + h.srv.URL + "/cli/confirm?code=ABCD-EFGH in your browser (or open it yourself). Compare the code on the page with the one above before you authorize.",
		"Waiting for approval...",
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.out)
		}
	}
	if !regexp.MustCompile(`Done\. This computer is ".+ \((Linux|macOS|Windows)\)" in Test mode\. Run listo login --live to add Live\.`).MatchString(r.out) {
		t.Errorf("no Done line:\n%s", r.out)
	}
	if !strings.Contains(r.out, "Saved in "+filepath.Join(h.dir, "credentials.json")+", because no system credential store answered.") {
		t.Errorf("does not say it used the file:\n%s", r.out)
	}
	if len(*opened) != 1 || (*opened)[0] != h.srv.URL+"/cli/confirm?code=ABCD-EFGH" {
		t.Errorf("browser opened %v", *opened)
	}
	if strings.Contains(r.out+r.err, "lso_at_") || strings.Contains(r.out+r.err, "lso_rt_") {
		t.Errorf("a token was printed:\n%s\n%s", r.out, r.err)
	}

	form := h.deviceForms[0]
	if form.Get("client_id") != "listo-cli" || form.Get("scope") != "read write" || form.Has("requested_mode") {
		t.Errorf("device request %v", form)
	}
	if name := form.Get("device_name"); name == "" || len([]rune(name)) > 64 {
		t.Errorf("device_name %q", name)
	}
	if len(h.tokenForms) != 3 || h.tokenForms[0].Get("device_code") != "lso_dc_FAKE" ||
		h.tokenForms[0].Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || h.tokenForms[0].Get("client_id") != "listo-cli" {
		t.Errorf("token polls %v", h.tokenForms)
	}

	rec := storedRecord(t, h.dir, config.Test)
	c := rec.Credential
	if !c.IsGrant() || c.AccessToken != "lso_at_1" || c.RefreshToken != "lso_rt_1" || c.DeviceName == "" || c.Mode != config.Test ||
		time.Until(c.AccessTokenExpiresAt) < 50*time.Minute {
		t.Errorf("stored %+v", c)
	}
	if got := storedRecord(t, h.dir, config.Live); got.Credential != nil {
		t.Errorf("live got a credential: %+v", got)
	}
	if info, err := os.Stat(filepath.Join(h.dir, "credentials.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("credentials file %v %v", info, err)
	}
}

func TestLoginLiveAsksForLiveAndStoresUnderTheModeThePageApproved(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	h.mode = "live"
	r := h.run(t, true, "", "login", "--live")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if h.deviceForms[0].Get("requested_mode") != "live" {
		t.Errorf("requested_mode: %v", h.deviceForms[0])
	}
	if !regexp.MustCompile(`in Live mode\. Run listo login to add Test\.`).MatchString(r.out) {
		t.Errorf("%s", r.out)
	}
	if storedRecord(t, h.dir, config.Live).Credential == nil || storedRecord(t, h.dir, config.Test).Credential != nil {
		t.Errorf("stored under the wrong mode")
	}

	// The person asked for Live and chose Test on the page: the grant is Test, and the CLI says so.
	h2 := newFakeHost(t)
	h2.mode = "test"
	r = h2.run(t, true, "", "login", "--live")
	if r.code != 0 || !strings.Contains(r.out, "Note: you asked for Live mode, and the page approved Test mode.") {
		t.Fatalf("%+v", r)
	}
	if storedRecord(t, h2.dir, config.Test).Credential == nil || storedRecord(t, h2.dir, config.Live).Credential != nil {
		t.Errorf("stored under the wrong mode")
	}
}

func TestLoginRefusalsNameTheNextStep(t *testing.T) {
	cases := []struct {
		name     string
		poll     string
		args     []string
		sentence string
		next     string
	}{
		{"denied", "access_denied", []string{"login"}, "The request was not allowed on the page.", "Next step: Run listo login."},
		{"expired", "expired_token", []string{"login"}, "The code expired before it was approved.", "Next step: Run listo login."},
		{"expired live", "expired_token", []string{"login", "--live"}, "The code expired before it was approved.", "Next step: Run listo login --live."},
		{"already used", "invalid_grant", []string{"login"}, "The sign-in host refused the request (invalid_grant: fake invalid_grant).", "Next step: Run listo login."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newFakeHost(t)
			stubSeams(t)
			h.polls = []string{"authorization_pending", c.poll}
			r := h.run(t, true, "", c.args...)
			if r.code != 1 {
				t.Fatalf("exit %d, want 1: %+v", r.code, r)
			}
			if !strings.Contains(r.err, c.sentence) || !strings.Contains(r.err, c.next+"\n") {
				t.Errorf("stderr:\n%s", r.err)
			}
			if storedRecord(t, h.dir, config.Test).Credential != nil {
				t.Errorf("a credential was stored")
			}
		})
	}
}

func TestLoginSlowDownIsObeyed(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	var waits []time.Duration
	pollSleep = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
	h.polls = []string{"slow_down", "authorization_pending", ""}
	if r := h.run(t, true, "", "login"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 10 * time.Second}
	if len(waits) != 3 || waits[0] != want[0] || waits[1] != want[1] || waits[2] != want[2] {
		t.Errorf("waits %v, want %v", waits, want)
	}
}

func TestLoginNonInteractivePrintsJSONThenCompleteStores(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	// stdin is not a terminal: non-interactive without being asked.
	r := h.run(t, false, "", "login")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if len(h.tokenForms) != 0 {
		t.Fatalf("it polled")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.out), &out); err != nil {
		t.Fatalf("%q: %v", r.out, err)
	}
	want := map[string]any{
		"verification_url": h.srv.URL + "/cli/confirm?code=ABCD-EFGH",
		"user_code":        "ABCD-EFGH",
		"expires_in":       float64(600),
		"next_step":        "listo login --complete lso_dc_FAKE",
	}
	if len(out) != len(want) {
		t.Errorf("keys: %v", out)
	}
	for k, v := range want {
		if out[k] != v {
			t.Errorf("%s = %v, want %v", k, out[k], v)
		}
	}
	if i, j := strings.Index(r.out, "verification_url"), strings.Index(r.out, "next_step"); i < 0 || j < i {
		t.Errorf("keys out of order: %s", r.out)
	}

	// The flag does the same on a terminal.
	if r := h.run(t, true, "", "login", "--non-interactive"); r.code != 0 || !strings.Contains(r.out, `"next_step"`) {
		t.Fatalf("%+v", r)
	}

	h.polls = []string{"authorization_pending", ""}
	r = h.run(t, false, "", "login", "--complete", "lso_dc_FAKE")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	var done map[string]string
	if err := json.Unmarshal([]byte(r.out), &done); err != nil {
		t.Fatalf("%q: %v", r.out, err)
	}
	if done["mode"] != "test" || done["credential"] != "sign_in" || done["partner"] != "Acme Delivery" || done["store"] != "file" || done["device"] == "" {
		t.Errorf("%v", done)
	}
	if got := h.tokenForms[0].Get("device_code"); got != "lso_dc_FAKE" {
		t.Errorf("polled with %q", got)
	}
	if storedRecord(t, h.dir, config.Test).Credential == nil {
		t.Errorf("nothing stored")
	}
	if strings.Contains(r.out+r.err, "lso_at_") {
		t.Errorf("a token was printed")
	}
}

func TestLoginWithAKey(t *testing.T) {
	h := newFakeHost(t)
	h.keys["lst_test_abc"] = "test"
	h.keys["lst_live_abc"] = "live"

	r := h.run(t, true, "", "login", "--key", "lst_test_abc")
	if r.code != 0 || !strings.Contains(r.out, "Done. This computer will use the Test API key of Acme Delivery.") {
		t.Fatalf("%+v", r)
	}
	if c := storedRecord(t, h.dir, config.Test).Credential; !c.IsKey() || c.APIKey != "lst_test_abc" {
		t.Errorf("%+v", c)
	}
	if strings.Contains(r.out+r.err, "lst_test_abc") {
		t.Errorf("the key was printed")
	}
	if len(h.pingTokens) != 1 || h.pingTokens[0] != "lst_test_abc" {
		t.Errorf("ping bearers %v", h.pingTokens)
	}

	for _, c := range []struct {
		name string
		args []string
		next string
		code int
	}{
		{"live key without --live", []string{"login", "--key", "lst_live_abc"}, "Add --live to use it.", 2},
		{"test key with --live", []string{"login", "--live", "--key", "lst_test_abc"}, "Remove --live", 2},
		{"not a key", []string{"login", "--key", "nope"}, "A key starts with lst_test_ or lst_live_.", 2},
		{"empty key", []string{"login", "--key", ""}, "A key starts with", 2},
		{"key and non-interactive", []string{"login", "--key", "lst_test_abc", "--non-interactive"}, "cannot be used together", 2},
		{"complete with no code", []string{"login", "--complete", ""}, "--complete needs the device code", 2},
		{"a key the API refuses", []string{"login", "--key", "lst_test_wrong"}, "Code: invalid_api_key", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h2 := newFakeHost(t)
			h2.keys["lst_test_abc"] = "test"
			r := h2.run(t, true, "", c.args...)
			if r.code != c.code || !strings.Contains(r.err, c.next) {
				t.Fatalf("%+v", r)
			}
			if storedRecord(t, h2.dir, config.Test).Credential != nil || storedRecord(t, h2.dir, config.Live).Credential != nil {
				t.Errorf("a credential was stored")
			}
		})
	}

	// A live key is stored under live, with --live.
	r = h.run(t, true, "", "login", "--live", "--key", "lst_live_abc")
	if r.code != 0 || !strings.Contains(r.out, "Live API key") || !storedRecord(t, h.dir, config.Live).Credential.IsKey() {
		t.Fatalf("%+v", r)
	}
}

func TestLoginReplacingAGrantRevokesTheOldOne(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	if r := h.run(t, true, "", "login"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if r := h.run(t, true, "", "login"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	if len(h.revokeForms) != 1 || h.revokeForms[0].Get("token") != "lso_rt_1" || h.revokeForms[0].Get("client_id") != "listo-cli" {
		t.Errorf("revoked %v", h.revokeForms)
	}
	if c := storedRecord(t, h.dir, config.Test).Credential; c.RefreshToken != "lso_rt_2" {
		t.Errorf("%+v", c)
	}
}

func TestLoginNeedsTheSignInHost(t *testing.T) {
	r := run(t, true, "", map[string]string{"LISTO_API_URL": "https://example.test:9"}, "login")
	if r.code != 1 || !strings.Contains(r.err, "The sign-in address cannot be worked out from https://example.test:9.") || !strings.Contains(r.err, "Next step: Set LISTO_LOGIN_URL.") {
		t.Fatalf("%+v", r)
	}
	// A key needs no sign-in host.
	h := newFakeHost(t)
	h.keys["lst_test_abc"] = "test"
	r = h.runEnv(t, true, "", map[string]string{"LISTO_LOGIN_URL": "", "LISTO_API_URL": h.srv.URL}, "login", "--key", "lst_test_abc")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestLoginInSpanish(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	r := h.run(t, true, "", "--lang", "es", "login")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{
		"Tu código es ABCD-EFGH",
		"Presiona Enter para abrir " + h.srv.URL + "/cli/confirm?code=ABCD-EFGH en tu navegador (o ábrela tú). Compara el código de la página con el de arriba antes de autorizar.",
		"Esperando la aprobación...",
		"en modo de prueba. Corre listo login --live para agregar el modo real.",
		"Guardado en " + filepath.Join(h.dir, "credentials.json"),
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.out)
		}
	}
	h.polls = []string{"access_denied"}
	r = h.run(t, true, "", "--lang", "es", "login")
	if r.code != 1 || !strings.Contains(r.err, "La solicitud no se autorizó en la página.") || !strings.Contains(r.err, "Siguiente paso: Corre listo login.") {
		t.Errorf("%+v", r)
	}
}

func TestLoginJSONResultWhenStdoutIsPiped(t *testing.T) {
	h := newFakeHost(t)
	stubSeams(t)
	// stdin is a terminal, stdout is not: the dialogue goes to stderr, the result to stdout.
	r := h.run(t, true, "", "login", "--json")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	// With a terminal for stdout the person sees text; --json forces the result as JSON on stdout.
	if !strings.Contains(r.err, "Your code is ABCD-EFGH") {
		t.Errorf("stderr: %s", r.err)
	}
	var done map[string]string
	if err := json.Unmarshal([]byte(r.out), &done); err != nil || done["mode"] != "test" {
		t.Fatalf("%q %v", r.out, err)
	}
}
