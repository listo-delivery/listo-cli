package cmd

import (
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newTriggerCmd()) }

// listPlaceholder stands for the list of event types in the help text, which
// comes from api.EventTypes (generated from the schema) and never from a
// catalog value, so a new published event shows up in the help by itself.
const listPlaceholder = "{list}"

func newTriggerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:       "trigger <event>",
		Short:     "trigger.short",
		Long:      "trigger.long",
		Example:   "trigger.example",
		Args:      output.Args(cobra.ExactArgs(1)),
		ValidArgs: api.EventTypes,
		RunE:      runTrigger,
	}
	c.Flags().Bool("deliver", false, "trigger.flag_deliver")
	withList(c, api.EventTypes)
	return c
}

// withList fills listPlaceholder in the command's help text with items, one per
// line, each time the help is drawn (the text itself is translated by Execute).
func withList(c *cobra.Command, items []string) {
	c.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		long := cmd.Long
		cmd.Long = strings.ReplaceAll(long, listPlaceholder, "  "+strings.Join(items, "\n  "))
		defer func() { cmd.Long = long }()
		cmd.Root().HelpFunc()(cmd, args)
	})
}

func runTrigger(c *cobra.Command, args []string) error {
	rt := Run(c)
	p := rt.Printer
	event := args[0]
	if !slices.Contains(api.EventTypes, event) {
		return output.Usage("trigger.unknown_event", i18n.Args{"type": event}).
			Next("trigger.next_list", i18n.Args{"types": strings.Join(api.EventTypes, ", ")})
	}
	deliver, _ := c.Flags().GetBool("deliver")

	client, err := rt.Client(c.Context())
	if err != nil {
		return err
	}
	// A live trigger is a write on the live account, and with --deliver it
	// reaches the live webhook URL, so it asks first like every live write.
	if rt.Live {
		key := "trigger.confirm_live_listen"
		if deliver {
			key = "trigger.confirm_live_deliver"
		}
		ok, err := p.Confirm(i18n.T(key, i18n.Args{"type": event}) + " " + i18n.T("deliveries.continue"))
		if err != nil {
			return err
		}
		if !ok {
			p.Declined()
			return nil
		}
	}

	ev, err := client.SendTestWebhook(c.Context(), api.TestWebhookRequest{Type: event, Deliver: &deliver})
	if err != nil {
		return err
	}
	if p.JSON {
		return p.PrintRawJSON(ev.Raw)
	}
	if deliver {
		p.Infof("trigger.sent_delivered", i18n.Args{"id": ev.ID})
	} else {
		p.Infof("trigger.sent", i18n.Args{"id": ev.ID})
	}
	return nil
}
