package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/logs"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newLogsCmd()) }

func newLogsCmd() *cobra.Command {
	parent := &cobra.Command{
		Use:     "logs",
		Short:   "logs.short",
		Long:    "logs.long",
		Example: "logs.example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	tail := &cobra.Command{
		Use:     "tail",
		Short:   "logs.tail_short",
		Long:    "logs.tail_long",
		Example: "logs.tail_example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runLogsTail,
	}
	tail.Flags().String("status", "", "logs.flag_status")
	tail.Flags().String("endpoint", "", "logs.flag_endpoint")
	tail.Flags().Bool("all", false, "logs.flag_all")
	parent.AddCommand(tail)
	return parent
}

func runLogsTail(c *cobra.Command, _ []string) error {
	rt := Run(c)
	p := rt.Printer
	ctx := c.Context()

	status, _ := c.Flags().GetString("status")
	endpoint, _ := c.Flags().GetString("endpoint")
	all, _ := c.Flags().GetBool("all")
	if status != "" && status != "failed" && status != "succeeded" {
		return output.Usage("logs.bad_status", i18n.Args{"status": status}).Next("logs.next_bad_status")
	}

	tokens, err := rt.TokenSource()
	if err != nil {
		return err
	}
	token, err := tokens.Token(ctx)
	if err != nil {
		return err
	}
	client := api.New(api.Options{BaseURL: rt.URLs.API, Tokens: tokens})

	s := &logs.Session{
		Client: client,
		Cable:  cable.Options{URL: rt.URLs.Cable, Tokens: tokens},
		Filter: logs.Filter{Status: status, Endpoint: endpoint, All: all, Token: token},
		Ready: func() {
			p.Infof("logs.ready", i18n.Args{"badge": p.Badge()})
		},
		Row:    func(l api.RequestLog) { printLog(p, l) },
		Notice: func(n cable.Notice) { printNotice(p, "logs", n) },
		CatchUp: func(missed int, err error) {
			switch {
			case err != nil:
				p.Infof("listen.catch_up_failed", i18n.Args{"detail": err.Error()})
			case missed > 0:
				p.Infof("listen.caught_up", i18n.Args{"count": missed})
			}
		},
	}
	return streamError(rt, "logs", s.Run(ctx))
}

// printLog prints one request: as the row the API lists in JSON mode (one
// object per line), else time, method, path, status (red from 400), duration,
// error code and request id.
func printLog(p *output.Printer, l api.RequestLog) {
	if p.JSON {
		_ = p.PrintJSON(l)
		return
	}
	at := "--:--:--"
	if t, err := time.Parse(time.RFC3339, l.CreatedAt); err == nil {
		at = t.Local().Format("15:04:05")
	}
	path := l.Endpoint
	if l.Path != nil && *l.Path != "" {
		path = *l.Path
	}
	code := fmt.Sprintf("%d", l.Status)
	if l.Status >= 400 {
		code = p.Red(code)
	} else {
		code = p.Green(code)
	}
	parts := []string{at, fmt.Sprintf("%-6s", l.Method), path, code}
	if l.DurationMS != nil {
		parts = append(parts, i18n.T("logs.duration", i18n.Args{"ms": *l.DurationMS}))
	}
	if l.ErrorCode != nil && *l.ErrorCode != "" {
		parts = append(parts, p.Red(*l.ErrorCode))
	}
	if l.RequestID != nil && *l.RequestID != "" {
		parts = append(parts, p.Dim(*l.RequestID))
	}
	fmt.Fprintln(p.Out, strings.Join(parts, "  "))
}
