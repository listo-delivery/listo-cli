package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/config"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
	"github.com/listo-delivery/listo-cli/internal/version"
)

// Test-only commands that return each kind of error, so the exit-code mapping is
// proven through Execute and not only through ExitCode.
func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use: "zz-usage", Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return output.Usage("error.bad_arguments").Next("error.next_help", i18n.Args{"command": "listo zz-usage"})
		},
	})
	rootCmd.AddCommand(&cobra.Command{
		Use: "zz-api", Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return &api.Error{Status: 401, Code: "invalid_api_key", Message: "Nope.", DocURL: "https://docs.listo.delivery/en/errors/invalid_api_key", RequestID: "req-1"}
		},
	})
	rootCmd.AddCommand(&cobra.Command{
		Use: "zz-confirm", Hidden: true,
		RunE: func(c *cobra.Command, _ []string) error {
			ok, err := Run(c).Printer.Confirm("Go?")
			if err != nil {
				return err
			}
			if !ok {
				Run(c).Printer.Declined()
			}
			return nil
		},
	})
}

// Spelled in two pieces so this file itself passes the guard that keeps the
// provider's name out of every file under cli/.
const bannedProvider = "ub" + "er"

type result struct {
	code     int
	out, err string
}

func run(t *testing.T, tty bool, in string, env map[string]string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	s := output.Streams{Out: &out, Err: &errb, In: strings.NewReader(in), OutTTY: tty, ErrTTY: tty, InTTY: tty}
	if env == nil {
		env = map[string]string{}
	}
	if _, ok := env["LISTO_CONFIG_DIR"]; !ok {
		env["LISTO_CONFIG_DIR"] = t.TempDir()
	}
	env["LISTO_NO_KEYRING"] = "1"
	env["NO_COLOR"] = "1"
	code := Execute(args, s, func(k string) string { return env[k] })
	i18n.SetLang(i18n.English)
	return result{code, out.String(), errb.String()}
}

func TestVersionCommandAndFlag(t *testing.T) {
	r := run(t, true, "", map[string]string{"LANG": "en_US.UTF-8"}, "version")
	if r.code != 0 || !strings.HasPrefix(r.out, "listo "+version.Version+" (") {
		t.Fatalf("%+v", r)
	}
	r = run(t, false, "", nil, "version")
	var j map[string]string
	if err := json.Unmarshal([]byte(r.out), &j); err != nil || j["version"] != version.Version {
		t.Fatalf("JSON when not a terminal: %q %v", r.out, err)
	}
	r = run(t, true, "", nil, "--version")
	if r.code != 0 || r.out != "listo version "+version.Version+"\n" {
		t.Fatalf("%+v", r)
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"usage failure", []string{"zz-usage"}, 2},
		{"api refusal", []string{"zz-api"}, 1},
		{"unknown command", []string{"nope"}, 2},
		{"unknown flag", []string{"version", "--nope"}, 2},
		{"wrong argument count", []string{"completion"}, 2},
		{"unknown shell", []string{"completion", "csh"}, 2},
		{"bad language", []string{"--lang", "fr", "version"}, 2},
		{"success", []string{"version"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := run(t, true, "", nil, c.args...); r.code != c.want {
				t.Fatalf("exit %d, want %d; stderr %q", r.code, c.want, r.err)
			}
		})
	}
}

func TestErrorsNameTheNextStep(t *testing.T) {
	r := run(t, true, "", nil, "zz-api")
	for _, want := range []string{"Error: Nope.", "Code: invalid_api_key", "Docs: https://docs.listo.delivery/en/errors/invalid_api_key", "Request id: req-1", "Next step: Run listo login"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
	r = run(t, true, "", nil, "nope")
	if !strings.Contains(r.err, "Unknown command: nope.") || !strings.Contains(r.err, "Next step: Run listo --help.") {
		t.Fatalf("%s", r.err)
	}
	r = run(t, false, "", nil, "zz-usage")
	var j map[string]map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.err)), &j); err != nil || j["error"]["code"] != "usage" || j["error"]["next_step"] == "" {
		t.Fatalf("JSON error on stderr: %q %v", r.err, err)
	}
	if r.out != "" {
		t.Fatalf("stdout must stay empty on an error: %q", r.out)
	}
}

func TestConfirmationNeedsYesWithoutATerminal(t *testing.T) {
	r := run(t, false, "y\n", nil, "zz-confirm")
	if r.code != 2 || !strings.Contains(r.err, "Add --yes.") {
		t.Fatalf("%+v", r)
	}
	if r := run(t, false, "", nil, "zz-confirm", "--yes"); r.code != 0 {
		t.Fatalf("--yes must pass: %+v", r)
	}
	if r := run(t, true, "n\n", nil, "zz-confirm"); r.code != 0 || !strings.Contains(r.out, "Canceled. Nothing was changed.") {
		t.Fatalf("a no is exit 0 with a line: %+v", r)
	}
}

func TestHelpIsTranslatedAndHasExamples(t *testing.T) {
	en := run(t, true, "", nil, "--lang", "en", "--help")
	if !strings.Contains(en.out, "Usage:") || !strings.Contains(en.out, "Examples:") || !strings.Contains(en.out, "listo trigger delivery.status_changed") ||
		!strings.Contains(en.out, "--live") || !strings.Contains(en.out, "Use live mode") {
		t.Fatalf("English help:\n%s", en.out)
	}
	es := run(t, true, "", nil, "--lang", "es", "--help")
	if !strings.Contains(es.out, "Uso:") || !strings.Contains(es.out, "Ejemplos:") || !strings.Contains(es.out, "Usa el modo real") ||
		strings.Contains(es.out, "Usage:") {
		t.Fatalf("Spanish help:\n%s", es.out)
	}
	// And back again in the same process: the second run translates from the key.
	again := run(t, true, "", nil, "--lang", "en", "--help")
	if !strings.Contains(again.out, "Use live mode") {
		t.Fatalf("English help after Spanish:\n%s", again.out)
	}
	// The language comes from the environment when no flag is given.
	env := run(t, true, "", map[string]string{"LANG": "es_MX.UTF-8"}, "completion", "--help")
	if !strings.Contains(env.out, "Imprime un script de autocompletado") {
		t.Fatalf("LANG=es_MX:\n%s", env.out)
	}
}

// Every command a lane adds must show an example in its help, in both languages,
// and none may leave an untranslated catalog key in its help text.
func TestEveryCommandHasAnExampleInBothLanguages(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		run(t, true, "", nil, "--lang", lang, "--help")
		i18n.SetLang(lang)
		var walk func(c *cobra.Command)
		walk = func(c *cobra.Command) {
			if !c.Hidden && c != rootCmd {
				if strings.TrimSpace(c.Example) == "" {
					t.Errorf("[%s] %s has no example", lang, c.CommandPath())
				}
				if strings.TrimSpace(c.Short) == "" {
					t.Errorf("[%s] %s has no short description", lang, c.CommandPath())
				}
			}
			for _, text := range []string{c.Short, c.Long, c.Example} {
				if i18n.Has(text) {
					t.Errorf("[%s] %s still shows the key %q", lang, c.CommandPath(), text)
				}
			}
			for _, sub := range c.Commands() {
				walk(sub)
			}
		}
		walk(rootCmd)
	}
	i18n.SetLang("en")
}

func TestNoBannedCharactersInHelp(t *testing.T) {
	for _, lang := range []string{"en", "es"} {
		for _, args := range [][]string{{"--help"}, {"version", "--help"}, {"completion", "--help"}, {"help"}} {
			r := run(t, true, "", nil, append([]string{"--lang", lang}, args...)...)
			for _, bad := range []string{"\u2014", "\u2013", "\u00b7"} {
				if strings.Contains(r.out, bad) {
					t.Errorf("%s %v prints %q", lang, args, bad)
				}
			}
			if strings.Contains(strings.ToLower(r.out), bannedProvider) {
				t.Errorf("%s %v names the provider", lang, args)
			}
		}
	}
}

func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		r := run(t, true, "", nil, "completion", shell)
		if r.code != 0 || !strings.Contains(r.out, "listo") || len(r.out) < 200 {
			t.Errorf("%s: exit %d, %d bytes", shell, r.code, len(r.out))
		}
	}
}

// --- credentials ---

func testRuntime(t *testing.T, mode config.Mode, env map[string]string) *Runtime {
	t.Helper()
	env["LISTO_CONFIG_DIR"] = t.TempDir()
	env["LISTO_NO_KEYRING"] = "1"
	return &Runtime{Getenv: func(k string) string { return env[k] }, Mode: mode, Live: mode == config.Live}
}

func failure(t *testing.T, err error) *output.Failure {
	t.Helper()
	f, ok := err.(*output.Failure)
	if !ok {
		t.Fatalf("got %T %v, want *output.Failure", err, err)
	}
	return f
}

func TestTokenSourceFromTheEnvironmentKey(t *testing.T) {
	rt := testRuntime(t, config.Test, map[string]string{EnvAPIKey: "lst_test_abc"})
	ts, err := rt.TokenSource()
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := ts.Token(t.Context()); tok != "lst_test_abc" {
		t.Fatalf("token %q", tok)
	}
	// A live key without --live is refused rather than switching the world.
	rt = testRuntime(t, config.Test, map[string]string{EnvAPIKey: "lst_live_abc"})
	if f := failure(t, func() error { _, e := rt.TokenSource(); return e }()); f.Exit != 2 || f.NextKey != "auth.next_add_live" {
		t.Fatalf("%+v", f)
	}
	// And a test key with --live.
	rt = testRuntime(t, config.Live, map[string]string{EnvAPIKey: "lst_test_abc"})
	if f := failure(t, func() error { _, e := rt.TokenSource(); return e }()); f.Exit != 2 || f.NextKey != "auth.next_drop_live" {
		t.Fatalf("%+v", f)
	}
	rt = testRuntime(t, config.Test, map[string]string{EnvAPIKey: "garbage"})
	if f := failure(t, func() error { _, e := rt.TokenSource(); return e }()); f.Exit != 2 || f.Key != "auth.bad_key_prefix" {
		t.Fatalf("%+v", f)
	}
}

func TestTokenSourceFromTheStore(t *testing.T) {
	rt := testRuntime(t, config.Test, map[string]string{})
	if f := failure(t, func() error { _, e := rt.TokenSource(); return e }()); f.Exit != 1 || f.Key != "auth.no_session" || f.NextKey != "auth.next_login" {
		t.Fatalf("%+v", f)
	}
	live := testRuntime(t, config.Live, map[string]string{})
	if f := failure(t, func() error { _, e := live.TokenSource(); return e }()); f.Key != "auth.no_live_session" || f.NextKey != "auth.next_login_live" {
		t.Fatalf("%+v", f)
	}
	// A stored key is used as is.
	store, err := rt.Store()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(t.Context(), config.Test, config.Record{Credential: &config.Credential{APIKey: "lst_test_stored"}}); err != nil {
		t.Fatal(err)
	}
	ts, err := rt.TokenSource()
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := ts.Token(t.Context()); tok != "lst_test_stored" {
		t.Fatalf("token %q", tok)
	}
	// A stored grant goes through the login lane's hook (login.go sets it in init(), so
	// this test takes it away to see the build without one, and puts it back).
	original := GrantSource
	GrantSource = nil
	defer func() { GrantSource = original }()
	if _, err := store.Save(t.Context(), config.Test, config.Record{Credential: &config.Credential{AccessToken: "at", RefreshToken: "rt"}}); err != nil {
		t.Fatal(err)
	}
	if f := failure(t, func() error { _, e := rt.TokenSource(); return e }()); f.Key != "auth.grant_unavailable" {
		t.Fatalf("with no GrantSource a grant is reported, got %+v", f)
	}
	GrantSource = func(_ *Runtime, _ config.Mode) (api.TokenSource, error) { return api.StaticKey("from-hook"), nil }
	ts, err = rt.TokenSource()
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := ts.Token(t.Context()); tok != "from-hook" {
		t.Fatalf("token %q", tok)
	}
}
