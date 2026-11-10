package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newLogoutCmd()) }

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		Short:   "logout.short",
		Long:    "logout.long",
		Example: "logout.example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runLogout,
	}
}

// logoutResult is the JSON of a logout.
type logoutResult struct {
	Mode      string `json:"mode"`
	LoggedOut bool   `json:"logged_out"`
	// Revoked is true when the server ended the session; false for a key (nothing
	// to end) and when the server could not be reached.
	Revoked bool `json:"revoked"`
}

// runLogout ends the selected mode's session: a grant is revoked on the server
// (RFC 7009, which always answers 200), then the credential AND that mode's
// webhook secret are removed from this computer. A key is only forgotten here.
func runLogout(c *cobra.Command, _ []string) error {
	rt := Run(c)
	p := rt.Printer
	store, err := rt.Store()
	if err != nil {
		return err
	}
	rec, err := store.Load(rt.Mode)
	if err != nil {
		return output.Fail("config.unreadable", i18n.Args{"detail": err.Error()}).Wrap(err)
	}
	mode := modeWord(rt.Mode)
	res := logoutResult{Mode: string(rt.Mode)}

	if rec.Credential == nil {
		if rec.WebhookSecret != "" {
			if err := store.Delete(c.Context(), rt.Mode); err != nil {
				return output.Fail("login.save_failed", i18n.Args{"detail": err.Error()}).Wrap(err)
			}
		}
		if p.JSON {
			return p.PrintJSON(res)
		}
		p.Infof("logout.nothing", i18n.Args{"mode": mode})
		return nil
	}

	switch {
	case rec.Credential.IsGrant() && rec.Credential.RefreshToken != "":
		res.Revoked, err = revokeGrant(c.Context(), rt, rec.Credential.RefreshToken)
		if err != nil {
			p.Infof("logout.server_unreachable", i18n.Args{"detail": err.Error()})
		}
	case rec.Credential.IsKey():
		// A key is the developer's own; nothing here can end it.
	}
	if err := store.Delete(c.Context(), rt.Mode); err != nil {
		return output.Fail("login.save_failed", i18n.Args{"detail": err.Error()}).Wrap(err)
	}
	res.LoggedOut = true
	if p.JSON {
		return p.PrintJSON(res)
	}
	p.Infof("logout.done", i18n.Args{"mode": mode})
	if rec.Credential.IsKey() {
		p.Infof("logout.key_note")
	}
	if rt.Getenv(EnvAPIKey) != "" {
		p.Infof("logout.env_note", i18n.Args{"env": EnvAPIKey})
	}
	return nil
}

// revokeGrant asks the sign-in host to end the grant. A host that cannot be
// reached is reported to the caller, which still removes the local copy.
func revokeGrant(ctx context.Context, rt *Runtime, refreshToken string) (bool, error) {
	flow, err := flowFor(rt)
	if err != nil {
		return false, errors.New(i18n.T("login.no_login_host", i18n.Args{"api": rt.URLs.API}))
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := flow.Revoke(rctx, refreshToken); err != nil {
		var te *api.TransportError
		if errors.As(err, &te) {
			return false, te
		}
		return false, err
	}
	return true, nil
}
