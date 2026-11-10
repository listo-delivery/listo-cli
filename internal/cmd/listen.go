package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/cable"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/listen"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newListenCmd()) }

func newListenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "listen",
		Short:   "listen.short",
		Long:    "listen.long",
		Example: "listen.example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runListen,
	}
	c.Flags().String("forward-to", "", "listen.flag_forward_to")
	c.Flags().StringSlice("events", nil, "listen.flag_events")
	c.Flags().Bool("print-secret", false, "listen.flag_print_secret")
	return c
}

// listenLine is one forwarded event in JSON output.
type listenLine struct {
	Time       string `json:"time"`
	Type       string `json:"type"`
	ID         string `json:"id"`
	Status     int    `json:"status,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func runListen(c *cobra.Command, _ []string) error {
	rt := Run(c)
	p := rt.Printer
	ctx := c.Context()

	printSecret, _ := c.Flags().GetBool("print-secret")
	target, _ := c.Flags().GetString("forward-to")
	events, _ := c.Flags().GetStringSlice("events")

	if !printSecret {
		if target == "" {
			return output.Usage("listen.need_forward_to").Next("listen.next_forward_to")
		}
		if u, err := url.Parse(target); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return output.Usage("listen.bad_url", i18n.Args{"url": target}).Next("listen.next_bad_url")
		}
	}
	types, err := eventFilter(events)
	if err != nil {
		return err
	}

	store, err := rt.Store()
	if err != nil {
		return err
	}
	secret, err := store.WebhookSecret(ctx, rt.Mode)
	if err != nil {
		return output.Fail("config.unreadable", i18n.Args{"detail": err.Error()}).Wrap(err)
	}
	if printSecret {
		// The secret alone, plain text even when piped: it is meant for
		// SECRET=$(listo listen --print-secret).
		_, err := fmt.Fprintln(p.Out, secret)
		return err
	}

	tokens, err := rt.TokenSource()
	if err != nil {
		return err
	}
	client := api.New(api.Options{BaseURL: rt.URLs.API, Tokens: tokens})

	s := &listen.Session{
		Client:    client,
		Cable:     cable.Options{URL: rt.URLs.Cable, Tokens: tokens},
		Forwarder: &listen.Forwarder{URL: target, Secret: secret},
		Types:     types,
		Ready: func() {
			p.Infof("listen.ready", i18n.Args{"badge": p.Badge(), "secret": secret})
		},
		Result: func(r listen.Result) { printResult(p, r) },
		Notice: func(n cable.Notice) { printNotice(p, "listen", n) },
		CatchUp: func(missed int, err error) {
			switch {
			case err != nil:
				p.Infof("listen.catch_up_failed", i18n.Args{"detail": err.Error()})
			case missed > 0:
				p.Infof("listen.caught_up", i18n.Args{"count": missed})
			}
		},
	}
	return streamError(rt, "listen", s.Run(ctx))
}

// eventFilter validates --events against the published event types.
func eventFilter(events []string) (map[string]bool, error) {
	if len(events) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	for _, t := range api.EventTypes {
		known[t] = true
	}
	out := map[string]bool{}
	for _, t := range events {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if !known[t] {
			return nil, output.Usage("listen.unknown_event", i18n.Args{"type": t}).
				Next("listen.next_event_types", i18n.Args{"types": strings.Join(api.EventTypes, ", ")})
		}
		out[t] = true
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func printResult(p *output.Printer, r listen.Result) {
	if p.JSON {
		l := listenLine{Time: r.Time.UTC().Format(time.RFC3339), Type: r.Type, ID: r.ID, Status: r.Status, DurationMS: r.Duration.Milliseconds()}
		if r.Err != nil {
			l.Error = r.Err.Error()
		}
		_ = p.PrintJSON(l)
		return
	}
	var tail string
	if r.Err != nil {
		tail = p.Red(i18n.T("listen.result_unreachable", i18n.Args{"url": r.URL, "detail": shortErr(r.Err)}))
	} else {
		tail = i18n.T("listen.result", i18n.Args{"status": r.Status, "ms": r.Duration.Milliseconds()})
		if !r.OK() {
			tail = p.Red(tail)
		}
	}
	fmt.Fprintf(p.Out, "%s  %s  %s  %s\n", r.Time.Local().Format("15:04:05"), r.Type, r.ID, tail)
}

// shortErr is the innermost message of a transport error: "connection refused",
// not the whole "Post "http://...": dial tcp ...: connect: connection refused".
func shortErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	return msg
}

// printNotice tells the person what the connection is doing. keyPrefix is
// "listen" or "logs" (both use the listen. wording for the connection; the
// argument exists so a later divergence is a catalog change only).
func printNotice(p *output.Printer, _ string, n cable.Notice) {
	secs := int(n.Wait.Round(time.Second) / time.Second)
	switch n.Kind {
	case cable.NoticeReconnecting:
		detail := "?"
		if n.Err != nil {
			detail = shortErr(n.Err)
		}
		if secs < 1 {
			secs = 1
		}
		p.Infof("listen.reconnecting", i18n.Args{"detail": detail, "seconds": secs})
	case cable.NoticeRateLimited:
		p.Infof("listen.rate_limited", i18n.Args{"seconds": secs})
	case cable.NoticeClosedByServer:
		p.Infof("listen.closed_by_server", i18n.Args{"reason": n.Reason})
	}
}

// streamError turns what a streaming session returned into the command's error.
// A cancelled context (Ctrl+C) is a clean stop.
func streamError(rt *Runtime, _ string, err error) error {
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return nil
	case errors.Is(err, cable.ErrKeyRevoked):
		next := "listen.next_key_revoked"
		if rt.Live {
			next = "listen.next_key_revoked_live"
		}
		return output.Fail("listen.key_revoked").WithCode("key_revoked").Next(next).Wrap(err)
	case errors.Is(err, cable.ErrSessionEnded):
		next := "auth.next_login"
		if rt.Live {
			next = "auth.next_login_live"
		}
		return output.Fail("listen.session_ended").WithCode("session_ended").Next(next).Wrap(err)
	case errors.Is(err, cable.ErrSubscriptionRejected):
		return output.Fail("listen.subscription_rejected").Next("listen.next_try_again").Wrap(err)
	}
	return err
}
