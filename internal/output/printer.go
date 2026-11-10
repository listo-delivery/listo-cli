// Package output is everything the CLI prints, and the one prompt it asks.
//
// A command gets a *Printer from its Runtime (see internal/cmd) and never
// writes to os.Stdout or os.Stderr itself, so the three rules of section 4.8
// hold in one place:
//
//   - JSON when --json is given OR stdout is not a terminal (Printer.JSON);
//     colour only on a terminal and never with NO_COLOR set (Printer.Color).
//   - Errors go to stderr, one fact per line (Printer.Error), or as one JSON
//     object on stderr in JSON mode; the exit code comes from ExitCode.
//   - A write in live mode asks first (Printer.Confirm), and with no terminal
//     and no --yes it refuses with exit 2 and says "Add --yes".
//
// Human sentences come from internal/i18n; this package only arranges them.
package output

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
)

// Streams are the three standard streams and whether each is a terminal.
type Streams struct {
	Out, Err io.Writer
	In       io.Reader
	OutTTY   bool
	ErrTTY   bool
	InTTY    bool
}

// StdStreams is the running process's streams, with terminal detection
// (an ioctl, so /dev/null is correctly not a terminal).
func StdStreams() Streams {
	return Streams{
		Out: os.Stdout, Err: os.Stderr, In: os.Stdin,
		OutTTY: term.IsTerminal(int(os.Stdout.Fd())),
		ErrTTY: term.IsTerminal(int(os.Stderr.Fd())),
		InTTY:  term.IsTerminal(int(os.Stdin.Fd())),
	}
}

// Options are the persistent flags that shape output.
type Options struct {
	JSON bool // --json
	Live bool // --live (selects the badge; credentials are the Runtime's business)
	Yes  bool // --yes
}

// Printer writes a command's output. Its exported fields are read-only to
// commands (tests may set them).
type Printer struct {
	Streams
	JSON     bool // JSON output: --json, or stdout is not a terminal
	Color    bool // colour on stdout
	ErrColor bool // colour on stderr
	Live     bool
	Yes      bool
}

// New builds a Printer. getenv is os.Getenv (a fake in tests): NO_COLOR set to
// anything non-empty, or TERM=dumb, turns colour off.
func New(s Streams, o Options, getenv func(string) string) *Printer {
	colorOK := getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
	return &Printer{
		Streams:  s,
		JSON:     o.JSON || !s.OutTTY,
		Color:    colorOK && s.OutTTY,
		ErrColor: colorOK && s.ErrTTY,
		Live:     o.Live,
		Yes:      o.Yes,
	}
}

// Info prints one human line. In JSON mode it goes to stderr instead, so stdout
// carries nothing but JSON.
func (p *Printer) Info(line string) {
	if p.JSON {
		fmt.Fprintln(p.Err, line)
		return
	}
	fmt.Fprintln(p.Out, line)
}

// Infof is Info with a catalog key: p.Infof("login.done", i18n.Args{...}).
func (p *Printer) Infof(key string, args ...i18n.Args) {
	p.Info(i18n.T(key, args...))
}

// Print writes JSON (or, for a command that has only text, a line) to stdout.
// Indented on a terminal, one compact line otherwise, so a stream of events is
// newline-delimited JSON a pipe can read line by line.
func (p *Printer) PrintJSON(v any) error {
	var (
		b   []byte
		err error
	)
	if p.OutTTY {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(p.Out, string(b))
	return err
}

// PrintRawJSON writes JSON bytes the server sent, unchanged but compacted onto
// one line when not on a terminal, so a command can show an API object without
// re-declaring its shape.
func (p *Printer) PrintRawJSON(raw []byte) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	return p.PrintJSON(v)
}

// Badge is "[TEST]" or "[LIVE]", coloured on a terminal: test is yellow, live
// is bold red, because live moves real money.
func (p *Printer) Badge() string {
	if p.Live {
		return p.Paint("[LIVE]", "1;31")
	}
	return p.Paint("[TEST]", "33")
}

// Paint wraps s in an ANSI colour when stdout is a terminal that wants colour.
func (p *Printer) Paint(s, code string) string {
	if !p.Color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (p *Printer) Red(s string) string    { return p.Paint(s, "31") }
func (p *Printer) Green(s string) string  { return p.Paint(s, "32") }
func (p *Printer) Yellow(s string) string { return p.Paint(s, "33") }
func (p *Printer) Dim(s string) string    { return p.Paint(s, "2") }
func (p *Printer) Bold(s string) string   { return p.Paint(s, "1") }

func (p *Printer) paintErr(s, code string) string {
	if !p.ErrColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// errorJSON is the shape of an error in JSON mode, printed to stderr.
type errorJSON struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Param     string `json:"param,omitempty"`
	DocURL    string `json:"doc_url,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
	NextStep  string `json:"next_step,omitempty"`
}

// Error prints err to stderr: for an API error its code, message, doc_url and
// request_id (one line each), for a Failure its message, then always the next
// step when one is known. In JSON mode it is one JSON object on stderr. It
// does not exit; the root command returns ExitCode(err).
func (p *Printer) Error(err error) {
	body := p.describe(err)
	if p.JSON {
		b, _ := json.Marshal(errorJSON{Error: body})
		fmt.Fprintln(p.Err, string(b))
		return
	}
	label := p.paintErr(i18n.T("error.label"), "1;31")
	fmt.Fprintf(p.Err, "%s %s\n", label, body.Message)
	if body.Code != "" && body.Code != "usage" && body.Code != "failed" {
		fmt.Fprintln(p.Err, i18n.T("error.code", i18n.Args{"code": body.Code}))
	}
	if body.Param != "" {
		fmt.Fprintln(p.Err, i18n.T("error.field", i18n.Args{"param": body.Param}))
	}
	if body.DocURL != "" {
		fmt.Fprintln(p.Err, i18n.T("error.docs", i18n.Args{"doc_url": body.DocURL}))
	}
	if body.RequestID != "" {
		fmt.Fprintln(p.Err, i18n.T("error.request_id", i18n.Args{"request_id": body.RequestID}))
	}
	if body.NextStep != "" {
		fmt.Fprintln(p.Err, i18n.T("error.next_step", i18n.Args{"step": body.NextStep}))
	}
}

func (p *Printer) describe(err error) errorBody {
	var f *Failure
	if errors.As(err, &f) {
		b := errorBody{Code: f.Code, Message: i18n.T(f.Key, f.Args)}
		if f.NextKey != "" {
			b.NextStep = i18n.T(f.NextKey, f.NextArgs)
		}
		return b
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		b := errorBody{Code: ae.Code, Message: ae.Message, Param: ae.Param, DocURL: ae.DocURL, RequestID: ae.RequestID, Retryable: ae.Retryable}
		if b.Message == "" {
			b.Message = i18n.T("error.http_status", i18n.Args{"status": ae.Status})
		}
		if key := api.NextStepKey(ae); key != "" {
			b.NextStep = i18n.T(key)
		}
		return b
	}
	var te *api.TransportError
	if errors.As(err, &te) {
		return errorBody{
			Code:     "network_error",
			Message:  i18n.T("error.network", i18n.Args{"detail": te.Error()}),
			NextStep: i18n.T("error.next_network"),
		}
	}
	return errorBody{Code: "failed", Message: strings.TrimSpace(err.Error())}
}

// Confirm asks a yes or no question before a write (section 4.6). The answer
// is "y" or "Y"; anything else, including Enter alone, is no. With --yes it
// answers yes without asking. With no terminal on stdin and no --yes it never
// proceeds: it returns a usage Failure (exit 2) whose next step is "Add --yes".
//
// A "no" returns (false, nil): the caller prints Declined() and does nothing,
// and exits 0, because the person chose it.
//
//	ok, err := p.Confirm(i18n.T("deliveries.confirm_book", args))
func (p *Printer) Confirm(question string) (bool, error) {
	if p.Yes {
		return true, nil
	}
	if !p.InTTY {
		return false, Usage("confirm.needs_terminal").WithCode("confirmation_needed").Next("confirm.next_yes")
	}
	// The prompt goes to stderr so a --json run still has a clean stdout.
	fmt.Fprintf(p.Err, "%s %s ", question, i18n.T("confirm.choices"))
	line, err := bufio.NewReader(p.In).ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	a := strings.TrimSpace(line)
	return a == "y" || a == "Y", nil
}

// Declined prints the line a command shows when the person answered no.
func (p *Printer) Declined() { p.Info(i18n.T("confirm.declined")) }
