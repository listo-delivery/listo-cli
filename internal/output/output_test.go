package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type rig struct {
	out, err bytes.Buffer
	p        *Printer
}

func newRig(tty bool, in string, o Options, e map[string]string) *rig {
	r := &rig{}
	r.p = New(Streams{Out: &r.out, Err: &r.err, In: strings.NewReader(in), OutTTY: tty, ErrTTY: tty, InTTY: tty}, o, env(e))
	return r
}

func TestJSONWhenFlaggedOrNotATerminal(t *testing.T) {
	if !newRig(false, "", Options{}, nil).p.JSON {
		t.Fatal("stdout that is not a terminal means JSON")
	}
	if newRig(true, "", Options{}, nil).p.JSON {
		t.Fatal("a terminal without --json means text")
	}
	if !newRig(true, "", Options{JSON: true}, nil).p.JSON {
		t.Fatal("--json means JSON")
	}
}

func TestColourOnlyOnATerminalAndNeverWithNoColor(t *testing.T) {
	if p := newRig(true, "", Options{}, nil).p; !p.Color || p.Red("x") != "\x1b[31mx\x1b[0m" {
		t.Fatalf("a terminal gets colour: %q", p.Red("x"))
	}
	if p := newRig(false, "", Options{}, nil).p; p.Color || p.Red("x") != "x" {
		t.Fatal("a pipe gets no colour")
	}
	if p := newRig(true, "", Options{}, map[string]string{"NO_COLOR": "1"}).p; p.Color || p.ErrColor || p.Red("x") != "x" {
		t.Fatal("NO_COLOR turns colour off")
	}
	if p := newRig(true, "", Options{}, map[string]string{"TERM": "dumb"}).p; p.Color {
		t.Fatal("TERM=dumb turns colour off")
	}
}

func TestBadge(t *testing.T) {
	if got := newRig(false, "", Options{}, nil).p.Badge(); got != "[TEST]" {
		t.Fatalf("got %q", got)
	}
	if got := newRig(false, "", Options{Live: true}, nil).p.Badge(); got != "[LIVE]" {
		t.Fatalf("got %q", got)
	}
	if got := newRig(true, "", Options{Live: true}, nil).p.Badge(); !strings.Contains(got, "[LIVE]") || !strings.Contains(got, "\x1b[") {
		t.Fatalf("live is coloured on a terminal: %q", got)
	}
}

func TestInfoGoesToStderrInJSONMode(t *testing.T) {
	r := newRig(false, "", Options{}, nil)
	r.p.Info("hello")
	if r.out.Len() != 0 || r.err.String() != "hello\n" {
		t.Fatalf("out %q err %q", r.out.String(), r.err.String())
	}
	r = newRig(true, "", Options{}, nil)
	r.p.Info("hello")
	if r.out.String() != "hello\n" || r.err.Len() != 0 {
		t.Fatalf("out %q err %q", r.out.String(), r.err.String())
	}
}

func TestPrintJSONIsCompactOffATerminal(t *testing.T) {
	r := newRig(false, "", Options{}, nil)
	if err := r.p.PrintJSON(map[string]any{"a": 1}); err != nil || r.out.String() != "{\"a\":1}\n" {
		t.Fatalf("%q %v", r.out.String(), err)
	}
	r = newRig(true, "", Options{JSON: true}, nil)
	_ = r.p.PrintJSON(map[string]any{"a": 1})
	if !strings.Contains(r.out.String(), "\n  \"a\": 1") {
		t.Fatalf("a terminal gets indented JSON: %q", r.out.String())
	}
}

func apiError() *api.Error {
	return &api.Error{Status: 402, Code: "insufficient_balance", Message: "Your balance is too low.", Param: "order_value_cents",
		DocURL: "https://docs.listo.delivery/en/errors/insufficient_balance", RequestID: "req-9"}
}

func TestAPIErrorPrintsOneFactPerLine(t *testing.T) {
	defer i18n.SetLang(i18n.English)
	i18n.SetLang(i18n.English)
	r := newRig(true, "", Options{}, map[string]string{"NO_COLOR": "1"})
	r.p.Error(apiError())
	want := "Error: Your balance is too low.\n" +
		"Code: insufficient_balance\n" +
		"Field: order_value_cents\n" +
		"Docs: https://docs.listo.delivery/en/errors/insufficient_balance\n" +
		"Request id: req-9\n" +
		"Next step: Add balance in the dashboard and try again.\n"
	if r.err.String() != want || r.out.Len() != 0 {
		t.Fatalf("got\n%s", r.err.String())
	}
	i18n.SetLang(i18n.Spanish)
	r = newRig(true, "", Options{}, map[string]string{"NO_COLOR": "1"})
	r.p.Error(apiError())
	if !strings.Contains(r.err.String(), "Siguiente paso: Agrega saldo en el panel") || !strings.Contains(r.err.String(), "Código: insufficient_balance") {
		t.Fatalf("Spanish output: %s", r.err.String())
	}
}

func TestErrorAsJSONOnStderr(t *testing.T) {
	r := newRig(false, "", Options{}, nil)
	r.p.Error(apiError())
	line := strings.TrimSpace(r.err.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("one JSON object, one line: %q", line)
	}
	var raw map[string]map[string]any
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatal(err)
	}
	e := raw["error"]
	for _, k := range []string{"code", "message", "param", "doc_url", "request_id", "next_step"} {
		if e[k] == nil || e[k] == "" {
			t.Errorf("JSON error lacks %q: %v", k, e)
		}
	}
	if r.out.Len() != 0 {
		t.Fatal("errors never go to stdout")
	}
}

func TestFailureAndTransportRendering(t *testing.T) {
	i18n.SetLang(i18n.English)
	r := newRig(true, "", Options{}, map[string]string{"NO_COLOR": "1"})
	r.p.Error(Usage("confirm.needs_terminal").Next("confirm.next_yes").WithCode("confirmation_needed"))
	if !strings.Contains(r.err.String(), "Error: This action needs your confirmation") || !strings.Contains(r.err.String(), "Code: confirmation_needed") || !strings.Contains(r.err.String(), "Next step: Add --yes.") {
		t.Fatalf("got %q", r.err.String())
	}
	r = newRig(true, "", Options{}, map[string]string{"NO_COLOR": "1"})
	r.p.Error(&api.TransportError{Err: errors.New("connection refused")})
	if !strings.Contains(r.err.String(), "Could not reach the API (connection refused).") || !strings.Contains(r.err.String(), "Next step: Check your connection") {
		t.Fatalf("got %q", r.err.String())
	}
}

func TestExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{Usage("error.bad_arguments"), 2},
		{Fail("auth.no_session"), 1},
		{apiError(), 1},
		{&api.TransportError{Err: errors.New("x")}, 1},
		{fmt.Errorf("wrapped: %w", Usage("error.bad_arguments")), 2},
		{fmt.Errorf("wrapped: %w", apiError()), 1},
		{errors.New(`unknown command "x" for "listo"`), 2},
		{errors.New("unknown flag: --nope"), 2},
		{errors.New("something else"), 1},
	}
	for _, c := range cases {
		if got := ExitCode(c.err); got != c.want {
			t.Errorf("ExitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

func TestArgsWrapperIsAUsageError(t *testing.T) {
	c := &cobra.Command{Use: "trigger"}
	err := Args(cobra.ExactArgs(1))(c, nil)
	var f *Failure
	if !errors.As(err, &f) || f.Exit != 2 || f.NextKey != "error.next_help" {
		t.Fatalf("got %v", err)
	}
	if Args(cobra.ExactArgs(1))(c, []string{"x"}) != nil {
		t.Fatal("the right count passes")
	}
}

func TestConfirm(t *testing.T) {
	i18n.SetLang(i18n.English)
	// --yes asks nothing.
	r := newRig(false, "", Options{Yes: true}, nil)
	if ok, err := r.p.Confirm("Book it?"); !ok || err != nil || r.err.Len() != 0 {
		t.Fatalf("--yes: %v %v %q", ok, err, r.err.String())
	}
	// No terminal and no --yes: refuse with exit 2 and say to add --yes.
	r = newRig(false, "y\n", Options{}, nil)
	ok, err := r.p.Confirm("Book it?")
	var f *Failure
	if ok || !errors.As(err, &f) || f.Exit != 2 || f.NextKey != "confirm.next_yes" || f.Code != "confirmation_needed" {
		t.Fatalf("no terminal: %v %v", ok, err)
	}
	if got := i18n.T(f.NextKey); got != "Add --yes." {
		t.Fatalf("next step reads %q", got)
	}
	// A terminal: y or Y is yes, Enter or anything else is no.
	for in, want := range map[string]bool{"y\n": true, "Y\n": true, "\n": false, "n\n": false, "yes\n": false, "": false} {
		r = newRig(true, in, Options{}, nil)
		got, err := r.p.Confirm("Book it?")
		if err != nil || got != want {
			t.Errorf("input %q: got %v %v, want %v", in, got, err, want)
		}
		if !strings.Contains(r.err.String(), "Book it? [y/N]") {
			t.Errorf("the prompt goes to stderr, got %q", r.err.String())
		}
		if r.out.Len() != 0 {
			t.Errorf("the prompt must not touch stdout")
		}
	}
}
