package output

import (
	"errors"
	"strings"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
)

// Exit codes (section 4.8 item 6): 0 success, 1 an API refusal or a failed
// operation, 2 a usage error. Nothing else is ever returned.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// Failure is the error a command returns when something went wrong that a
// person should read about. Build one with Usage or Fail, name the next step
// with Next, and RETURN it from RunE: the root command prints it (one line per
// fact, or JSON on stderr) and exits with its code. A command never prints an
// error and calls os.Exit itself.
//
//	return output.Usage("trigger.unknown_event", i18n.Args{"type": t}).
//		Next("trigger.next_list")
//
// Every Failure should name the next step ("Run listo login", "Add --yes").
type Failure struct {
	Exit     int
	Code     string // machine name for JSON output, e.g. "confirmation_needed"; may be empty
	Key      string // catalog key of the message
	Args     i18n.Args
	NextKey  string // catalog key of the next step, may be empty
	NextArgs i18n.Args
	Cause    error
}

// Usage is a usage error: the person typed the command wrong. Exit 2.
func Usage(key string, args ...i18n.Args) *Failure {
	return &Failure{Exit: ExitUsage, Code: "usage", Key: key, Args: merge(args)}
}

// Fail is a refusal or a failed operation that is not an API error. Exit 1.
func Fail(key string, args ...i18n.Args) *Failure {
	return &Failure{Exit: ExitFailure, Code: "failed", Key: key, Args: merge(args)}
}

// Next names what the person should do now, as a catalog key.
func (f *Failure) Next(key string, args ...i18n.Args) *Failure {
	f.NextKey, f.NextArgs = key, merge(args)
	return f
}

// WithCode sets the machine-readable code printed in JSON output.
func (f *Failure) WithCode(code string) *Failure {
	f.Code = code
	return f
}

// Wrap keeps the underlying error for errors.Is / errors.As; it is never printed.
func (f *Failure) Wrap(err error) *Failure {
	f.Cause = err
	return f
}

func (f *Failure) Error() string { return i18n.T(f.Key, f.Args) }
func (f *Failure) Unwrap() error { return f.Cause }

func merge(args []i18n.Args) i18n.Args {
	if len(args) == 0 {
		return nil
	}
	out := i18n.Args{}
	for _, a := range args {
		for k, v := range a {
			out[k] = v
		}
	}
	return out
}

// ExitCode maps any error a command returned to the process exit code.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var f *Failure
	if errors.As(err, &f) {
		return f.Exit
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ExitFailure
	}
	if isCobraUsageError(err) {
		return ExitUsage
	}
	return ExitFailure
}

// isCobraUsageError recognises what cobra and pflag return for a command line
// they could not parse. They return plain errors with no type, so this is the
// one place the wording is matched; the root command also converts flag errors
// into Usage failures before they get here, which is the reliable path.
func isCobraUsageError(err error) bool {
	m := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand flag", "invalid argument", "required flag", "flag needs an argument", "accepts ", "requires at least", "requires at most"} {
		if strings.HasPrefix(m, p) || strings.Contains(m, p) {
			return true
		}
	}
	return false
}

// Args wraps a cobra Args validator so a wrong argument count is a usage error
// (exit 2) naming the command's help as the next step.
//
//	Args: output.Args(cobra.ExactArgs(1)),
func Args(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return Usage("error.bad_arguments").
				Next("error.next_help", i18n.Args{"command": cmd.CommandPath()})
		}
		return nil
	}
}
