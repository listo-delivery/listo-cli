package cmd

import (
	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/config"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newWhoamiCmd()) }

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "whoami",
		Short:   "whoami.short",
		Long:    "whoami.long",
		Example: "whoami.example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runWhoami,
	}
}

type whoamiOut struct {
	Partner struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"partner"`
	Mode       string `json:"mode"`
	Credential string `json:"credential"` // "sign_in", "key" or "env_key"
	Device     string `json:"device,omitempty"`
}

// runWhoami asks the API who the credential is (GET /v1/ping). The mode printed
// is the API's own answer, so it is true for a grant too, whose mode is not in
// its token.
func runWhoami(c *cobra.Command, _ []string) error {
	rt := Run(c)
	p := rt.Printer
	client, err := rt.Client(c.Context())
	if err != nil {
		return err
	}
	ping, err := client.Ping(c.Context())
	if err != nil {
		return err
	}
	out := whoamiOut{Mode: ping.Mode}
	out.Partner.Slug, out.Partner.Name = ping.Partner.Slug, ping.Partner.Name

	if rt.Getenv(EnvAPIKey) != "" {
		out.Credential = "env_key"
	} else if store, serr := rt.Store(); serr == nil {
		if rec, lerr := store.Load(rt.Mode); lerr == nil {
			if rec.Credential.IsGrant() {
				out.Credential, out.Device = "sign_in", rec.Credential.DeviceName
			} else {
				out.Credential = "key"
			}
		}
	}
	if p.JSON {
		return p.PrintJSON(out)
	}
	p.Infof("whoami.partner", i18n.Args{"name": out.Partner.Name, "slug": out.Partner.Slug})
	p.Infof("whoami.mode", i18n.Args{"badge": modeBadge(p, config.Mode(ping.Mode))})
	switch out.Credential {
	case "sign_in":
		p.Infof("whoami.sign_in", i18n.Args{"device": out.Device})
	case "env_key":
		p.Infof("whoami.env_key", i18n.Args{"env": EnvAPIKey})
	default:
		p.Infof("whoami.key")
	}
	return nil
}

// modeBadge is [TEST] or [LIVE] by the mode the API answered, not by --live.
func modeBadge(p *output.Printer, m config.Mode) string {
	if m == config.Live {
		return p.Paint("[LIVE]", "1;31")
	}
	return p.Paint("[TEST]", "33")
}
