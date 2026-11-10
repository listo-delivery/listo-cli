package cmd

import (
	"bufio"
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/config"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/login"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() {
	rootCmd.AddCommand(newLoginCmd())
	GrantSource = newGrantSource
}

// Seams the tests replace: the browser, the wait between polls and the clock.
var (
	openBrowser = login.OpenBrowser
	pollSleep   = login.Sleep
	loginNow    = time.Now
)

func newLoginCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "login",
		Short:   "login.short",
		Long:    "login.long",
		Example: "login.example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runLogin,
	}
	c.Flags().String("key", "", "login.flag_key")
	c.Flags().Bool("non-interactive", false, "login.flag_non_interactive")
	c.Flags().String("complete", "", "login.flag_complete")
	return c
}

// flowFor is the sign-in host's client, or the refusal that names how to set it.
func flowFor(rt *Runtime) (*login.Flow, error) {
	if rt.URLs.Login == "" {
		return nil, output.Fail("login.no_login_host", i18n.Args{"api": rt.URLs.API}).Next("login.next_set_login_url")
	}
	return &login.Flow{LoginURL: rt.URLs.Login, Sleep: pollSleep, Now: loginNow}, nil
}

// newGrantSource is the GrantSource hook: the refreshing token source of a stored grant.
func newGrantSource(rt *Runtime, mode config.Mode) (api.TokenSource, error) {
	flow, err := flowFor(rt)
	if err != nil {
		return nil, err
	}
	store, err := rt.Store()
	if err != nil {
		return nil, err
	}
	return &login.GrantSource{Store: store, Mode: mode, Flow: flow, Now: loginNow}, nil
}

// result is what login prints as JSON when it is done.
type loginResult struct {
	Mode       string `json:"mode"`
	Credential string `json:"credential"` // "sign_in" or "key"
	Device     string `json:"device,omitempty"`
	Partner    string `json:"partner"`
	Store      string `json:"store"` // "keyring" or "file"
}

// nonInteractiveOut is the JSON of --non-interactive.
type nonInteractiveOut struct {
	VerificationURL string `json:"verification_url"`
	UserCode        string `json:"user_code"`
	ExpiresIn       int    `json:"expires_in"`
	NextStep        string `json:"next_step"`
}

func runLogin(c *cobra.Command, _ []string) error {
	rt := Run(c)
	key, _ := c.Flags().GetString("key")
	nonInteractive, _ := c.Flags().GetBool("non-interactive")
	complete, _ := c.Flags().GetString("complete")
	completeGiven := c.Flags().Changed("complete")

	given := 0
	for _, on := range []bool{key != "" || c.Flags().Changed("key"), nonInteractive, completeGiven} {
		if on {
			given++
		}
	}
	if given > 1 {
		return output.Usage("login.conflicting_flags").Next("error.next_help", i18n.Args{"command": c.CommandPath()})
	}
	if c.Flags().Changed("key") {
		return loginWithKey(c.Context(), rt, key)
	}
	if completeGiven {
		if complete == "" {
			return output.Usage("login.complete_needs_code").Next("error.next_help", i18n.Args{"command": c.CommandPath()})
		}
		return loginComplete(c.Context(), rt, complete)
	}
	return loginInteractive(c.Context(), rt, nonInteractive || !rt.Printer.InTTY)
}

// loginInteractive is `listo login`: ask for a code, show it, wait for the person.
func loginInteractive(ctx context.Context, rt *Runtime, nonInteractive bool) error {
	p := rt.Printer
	flow, err := flowFor(rt)
	if err != nil {
		return err
	}
	store, err := rt.Store()
	if err != nil {
		return err
	}
	device := login.DeviceName()
	asked := ""
	if rt.Live {
		asked = "live"
	}
	dc, err := flow.RequestDeviceCode(ctx, device, asked)
	if err != nil {
		return refusal(err)
	}

	if nonInteractive {
		return p.PrintJSON(nonInteractiveOut{
			VerificationURL: dc.URL(),
			UserCode:        dc.UserCode,
			ExpiresIn:       dc.ExpiresIn,
			NextStep:        "listo login --complete " + dc.DeviceCode,
		})
	}

	p.Infof("login.code", i18n.Args{"code": dc.UserCode})
	p.Infof("login.press_enter", i18n.Args{"url": dc.URL()})
	p.Infof("login.waiting")

	type polled struct {
		tok *login.Tokens
		err error
	}
	done := make(chan polled, 1)
	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		tok, err := flow.Poll(pollCtx, dc.DeviceCode, dc.ExpiresIn, dc.Interval)
		done <- polled{tok, err}
	}()

	var enter chan struct{}
	if p.InTTY {
		enter = make(chan struct{}, 1)
		go func() {
			line, err := bufio.NewReader(p.In).ReadString('\n')
			if err == nil || line != "" {
				enter <- struct{}{}
			}
		}()
	}
	var res polled
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-enter:
			enter = nil
			if err := openBrowser(dc.URL()); err != nil {
				p.Infof("login.browser_failed")
			}
		}
	}
	if res.err != nil {
		return pollFailure(res.err, rt.Mode)
	}
	return finishLogin(ctx, rt, store, res.tok, device, asked)
}

// loginComplete is `listo login --complete <device_code>`: poll a code another
// shell asked for. The device name is this computer's; the mode comes from the
// grant itself.
func loginComplete(ctx context.Context, rt *Runtime, deviceCode string) error {
	p := rt.Printer
	flow, err := flowFor(rt)
	if err != nil {
		return err
	}
	store, err := rt.Store()
	if err != nil {
		return err
	}
	p.Infof("login.waiting")
	tok, err := flow.Poll(ctx, deviceCode, 0, 0)
	if err != nil {
		return pollFailure(err, rt.Mode)
	}
	asked := ""
	if rt.LiveGiven && rt.Live {
		asked = "live"
	}
	return finishLogin(ctx, rt, store, tok, login.DeviceName(), asked)
}

// finishLogin asks /v1/ping which mode the approval was for, stores the grant
// under that mode and says so.
func finishLogin(ctx context.Context, rt *Runtime, store *config.Store, tok *login.Tokens, device, asked string) error {
	p := rt.Printer
	probe := api.New(api.Options{BaseURL: rt.URLs.API, Tokens: api.StaticKey(tok.AccessToken)})
	ping, err := probe.Ping(ctx)
	if err != nil {
		// A grant nobody can use is not left behind on the server.
		if flow, ferr := flowFor(rt); ferr == nil {
			_ = flow.Revoke(context.WithoutCancel(ctx), tok.RefreshToken)
		}
		return err
	}
	mode := config.Mode(ping.Mode)
	if mode != config.Test && mode != config.Live {
		return output.Fail("login.unknown_mode", i18n.Args{"mode": ping.Mode})
	}
	cred := &config.Credential{
		AccessToken:          tok.AccessToken,
		AccessTokenExpiresAt: tok.ExpiresAt,
		RefreshToken:         tok.RefreshToken,
		DeviceName:           device,
	}
	backend, err := saveCredential(ctx, rt, store, mode, cred)
	if err != nil {
		return err
	}
	if asked != "" && asked != string(mode) {
		p.Infof("login.mode_differs", i18n.Args{"asked": modeWord(config.Mode(asked)), "got": modeWord(mode)})
	}
	if p.JSON {
		return p.PrintJSON(loginResult{Mode: string(mode), Credential: "sign_in", Device: device, Partner: ping.Partner.Name, Store: backend})
	}
	if mode == config.Live {
		p.Infof("login.done_live", i18n.Args{"device": device})
	} else {
		p.Infof("login.done_test", i18n.Args{"device": device})
	}
	p.Info(storedLine(store, backend))
	return nil
}

// loginWithKey is `listo login --key lst_...`.
func loginWithKey(ctx context.Context, rt *Runtime, key string) error {
	p := rt.Printer
	mode, ok := api.KeyMode(key)
	if !ok {
		return output.Usage("auth.bad_key_prefix", i18n.Args{"env": "--key"}).Next("auth.next_key_prefix")
	}
	if config.Mode(mode) != rt.Mode {
		if mode == "live" {
			return output.Usage("auth.live_key_needs_flag").Next("auth.next_add_live")
		}
		return output.Usage("auth.test_key_with_live").Next("auth.next_drop_live")
	}
	store, err := rt.Store()
	if err != nil {
		return err
	}
	probe := api.New(api.Options{BaseURL: rt.URLs.API, Tokens: api.StaticKey(key)})
	ping, err := probe.Ping(ctx)
	if err != nil {
		return err
	}
	backend, err := saveCredential(ctx, rt, store, config.Mode(mode), &config.Credential{APIKey: key})
	if err != nil {
		return err
	}
	if p.JSON {
		return p.PrintJSON(loginResult{Mode: mode, Credential: "key", Partner: ping.Partner.Name, Store: backend})
	}
	p.Infof("login.done_key", i18n.Args{"mode": modeWord(config.Mode(mode)), "partner": ping.Partner.Name})
	p.Info(storedLine(store, backend))
	return nil
}

// saveCredential replaces the mode's credential (keeping its webhook secret) and
// ends, best effort, a grant it replaced, so a second login does not leave the
// first computer session alive on the server.
func saveCredential(ctx context.Context, rt *Runtime, store *config.Store, mode config.Mode, cred *config.Credential) (string, error) {
	var oldRefresh string
	backend, err := store.Update(ctx, mode, func(r *config.Record) error {
		if r.Credential.IsGrant() {
			oldRefresh = r.Credential.RefreshToken
		}
		r.Credential = cred
		return nil
	})
	if err != nil {
		return "", output.Fail("login.save_failed", i18n.Args{"detail": err.Error()}).Wrap(err)
	}
	if oldRefresh != "" && oldRefresh != cred.RefreshToken {
		revokeQuietly(ctx, rt, oldRefresh)
	}
	return backend, nil
}

func revokeQuietly(ctx context.Context, rt *Runtime, refreshToken string) {
	flow, err := flowFor(rt)
	if err != nil {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = flow.Revoke(rctx, refreshToken)
}

func storedLine(store *config.Store, backend string) string {
	if backend == config.BackendKeyring {
		return i18n.T("login.stored_keyring")
	}
	return i18n.T("login.stored_file", i18n.Args{"path": filepath.Join(store.Dir(), "credentials.json")})
}

func modeWord(m config.Mode) string {
	if m == config.Live {
		return i18n.T("login.mode_live")
	}
	return i18n.T("login.mode_test")
}

// pollFailure turns what ended the wait into a sentence with its next step.
func pollFailure(err error, mode config.Mode) error {
	next := "auth.next_login"
	if mode == config.Live {
		next = "auth.next_login_live"
	}
	switch {
	case errors.Is(err, login.ErrDenied):
		return output.Fail("login.denied").WithCode("access_denied").Next(next)
	case errors.Is(err, login.ErrExpired):
		return output.Fail("login.expired").WithCode("expired_token").Next(next)
	}
	return refusal(err)
}

// refusal turns an OAuth error answer into a Failure; anything else (a canceled
// context, no answer at all) passes through for the root command to describe.
func refusal(err error) error {
	var oe *login.OAuthError
	if errors.As(err, &oe) {
		detail := oe.Code
		if oe.Description != "" {
			detail += ": " + oe.Description
		}
		if detail == "" {
			detail = i18n.T("error.http_status", i18n.Args{"status": oe.Status})
		}
		f := output.Fail("login.rejected", i18n.Args{"detail": detail}).Wrap(err)
		if oe.Code != "" {
			f = f.WithCode(oe.Code)
		}
		return f.Next("auth.next_login")
	}
	return err
}
