// Package cmd is the cobra command tree of the `listo` CLI.
//
// HOW A COMMAND IS ADDED (every command lane follows this, and no two lanes
// ever edit the same file):
//
//  1. One file per command, internal/cmd/<name>.go.
//
//  2. The file registers itself in its own init():
//
//     func init() { rootCmd.AddCommand(newTriggerCmd()) }
//
//  3. Help text is written as CATALOG KEYS, not sentences. At startup Execute
//     replaces every Short, Long, Example and flag usage that is exactly a key of
//     internal/i18n/en.json with the text in the person's language, so the help
//     is translated and the tree can still be built in init():
//
//     &cobra.Command{
//     Use:     "trigger <event>",
//     Short:   "trigger.short",
//     Example: "trigger.example", // EVERY command has an example
//     Args:    output.Args(cobra.ExactArgs(1)),
//     RunE:    func(c *cobra.Command, args []string) error { ... },
//     }
//     c.Flags().Bool("deliver", false, "trigger.flag_deliver")
//
//  4. RunE gets what it needs from the Runtime (runtime.go): Run(c).Printer
//     for output, Run(c).Client(ctx) for an authenticated API client in the
//     right mode, Run(c).Store() for the credential store, Run(c).URLs for the
//     three hosts. A command never reads os.Args, the environment, os.Stdout or
//     the credential store itself, and never calls os.Exit: it RETURNS an error.
//
//  5. Errors: return output.Usage(...) (exit 2) or output.Fail(...) (exit 1)
//     with .Next(...) naming what to do, or return the *api.Error the client
//     gave you. Execute prints it and maps the exit code (0 success, 1 an API
//     refusal or a failed operation, 2 a usage error).
//
//  6. Do not define PersistentPreRun(E) on a command; use RunE.
//
// The persistent flags are on the root: --live, --yes, --json, --lang.
package cmd

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
	"github.com/listo-delivery/listo-cli/internal/version"
)

var rootCmd = &cobra.Command{
	Use:           "listo",
	Short:         "root.short",
	Long:          "root.long",
	Example:       "root.example",
	Version:       version.Version,
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	RunE: func(c *cobra.Command, args []string) error {
		return c.Help()
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.Bool("live", false, "flag.live")
	pf.Bool("yes", false, "flag.yes")
	pf.Bool("json", false, "flag.json")
	pf.String("lang", "", "flag.lang")

	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.SetVersionTemplate("listo version {{.Version}}\n")
	rootCmd.SetHelpCommand(&cobra.Command{
		Use:     "help [command]",
		Short:   "help.short",
		Example: "help.example",
		RunE: func(c *cobra.Command, args []string) error {
			target, _, err := c.Root().Find(args)
			if err != nil || target == nil {
				return output.Usage("error.unknown_command", i18n.Args{"name": strings.Join(args, " ")}).
					Next("error.next_help", i18n.Args{"command": "listo"})
			}
			return target.Help()
		},
	})
	rootCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return output.Usage("error.bad_flag", i18n.Args{"detail": err.Error()}).
			Next("error.next_help", i18n.Args{"command": c.CommandPath()})
	})

	cobra.AddTemplateFunc("t", func(key string) string { return i18n.T(key) })
	cobra.AddTemplateFunc("tcmd", func(key, path string) string { return i18n.T(key, i18n.Args{"command": path}) })
	rootCmd.SetUsageTemplate(usageTemplate)
}

// usageTemplate is cobra's default with every fixed word looked up in the catalog.
const usageTemplate = `{{t "usage.usage"}}{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{if gt (len .Aliases) 0}}

{{t "usage.aliases"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{t "usage.examples"}}
{{.Example}}{{end}}{{if .HasAvailableSubCommands}}

{{t "usage.commands"}}{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{t "usage.flags"}}
{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

{{t "usage.global_flags"}}
{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableSubCommands}}

{{tcmd "usage.more" .CommandPath}}{{end}}
`

// Execute runs the CLI and returns the process exit code. main.go is
// os.Exit(cmd.Execute(os.Args[1:], output.StdStreams(), os.Getenv)).
func Execute(args []string, streams output.Streams, getenv func(string) string) int {
	state = execState{streams: streams, getenv: getenv}
	resetFlags(rootCmd)
	resetContext(rootCmd)

	flagLang, langErr := scanLang(args)
	i18n.SetLang(i18n.Detect(flagLang, i18n.SystemEnv(getenv)))
	localize(rootCmd)

	rootCmd.SetArgs(args)
	rootCmd.SetOut(streams.Out)
	rootCmd.SetErr(streams.Err)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	if langErr != nil {
		err = langErr
	} else {
		err = rootCmd.ExecuteContext(ctx)
	}
	if err == nil {
		return output.ExitOK
	}
	if errors.Is(err, context.Canceled) {
		return output.ExitFailure
	}
	err = classify(err)
	currentRuntime().Printer.Error(err)
	return output.ExitCode(err)
}

var langFlag = regexp.MustCompile(`\A--lang(?:=(.*))?\z`)

// scanLang finds --lang in the raw arguments BEFORE cobra parses them, because
// the language must be set before any help text or usage is rendered. An invalid
// value is a usage error.
func scanLang(args []string) (string, error) {
	for i, a := range args {
		if a == "--" {
			break
		}
		m := langFlag.FindStringSubmatch(a)
		if m == nil {
			continue
		}
		v := m[1]
		if !strings.Contains(a, "=") {
			if i+1 >= len(args) {
				return "", output.Usage("error.bad_lang", i18n.Args{"value": ""}).Next("error.next_lang")
			}
			v = args[i+1]
		}
		if !i18n.ValidFlag(v) {
			return "", output.Usage("error.bad_lang", i18n.Args{"value": v}).Next("error.next_lang")
		}
		return v, nil
	}
	return "", nil
}

var unknownCommand = regexp.MustCompile(`unknown command "([^"]*)"`)

// classify turns the untyped errors cobra returns for a command line it could
// not use into usage failures (exit 2). Typed errors pass through.
func classify(err error) error {
	var f *output.Failure
	if errors.As(err, &f) {
		return err
	}
	if m := unknownCommand.FindStringSubmatch(err.Error()); m != nil {
		return output.Usage("error.unknown_command", i18n.Args{"name": m[1]}).
			Next("error.next_help", i18n.Args{"command": "listo"})
	}
	if output.ExitCode(err) == output.ExitUsage {
		return output.Usage("error.bad_flag", i18n.Args{"detail": err.Error()}).
			Next("error.next_help", i18n.Args{"command": "listo"})
	}
	return err
}

// resetFlags puts every flag back to its default, so a second Execute in the
// same process (the tests) starts clean.
func resetFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			_ = sv.Replace(nil)
		} else {
			_ = f.Value.Set(f.DefValue)
		}
		f.Changed = false
	}
	c.Flags().VisitAll(reset)
	c.PersistentFlags().VisitAll(reset)
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

// resetContext forgets the context cobra cached on each command. Cobra hands a
// subcommand the context of the FIRST run and keeps it, so a second Execute in
// the same process (the tests) would run its commands under a context the first
// run already canceled.
func resetContext(c *cobra.Command) {
	c.SetContext(nil)
	for _, sub := range c.Commands() {
		resetContext(sub)
	}
}

// localize replaces catalog keys in help text with the current language.
func localize(c *cobra.Command) {
	c.InitDefaultHelpFlag()
	if c == rootCmd {
		// Adds the custom help command to the tree so it is translated below.
		c.InitDefaultHelpCmd()
	}
	if f := c.Flags().Lookup("help"); f != nil {
		f.Usage = "flag.help"
	}
	if c == rootCmd {
		c.InitDefaultVersionFlag()
		if f := c.Flags().Lookup("version"); f != nil {
			f.Usage = "flag.version"
		}
	}
	src, seen := helpKeys[c]
	if !seen {
		src = helpSource{short: c.Short, long: c.Long, example: c.Example}
		helpKeys[c] = src
	}
	c.Short, c.Long, c.Example = tr(src.short), tr(src.long), tr(src.example)
	fix := func(f *pflag.Flag) {
		key, seen := flagKeys[f]
		if !seen {
			key = f.Usage
			flagKeys[f] = key
		}
		f.Usage = tr(key)
	}
	c.Flags().VisitAll(fix)
	c.PersistentFlags().VisitAll(fix)
	for _, sub := range c.Commands() {
		localize(sub)
	}
}

// What each command and flag was declared with, so a second Execute in the same
// process (the tests run both languages) translates from the key again instead
// of from the sentence the first run left in the field.
type helpSource struct{ short, long, example string }

var (
	helpKeys = map[*cobra.Command]helpSource{}
	flagKeys = map[*pflag.Flag]string{}
)

// tr is T for a value that may be a catalog key; anything else is kept.
func tr(s string) string {
	if s != "" && i18n.Has(s) {
		return i18n.T(s)
	}
	return s
}
