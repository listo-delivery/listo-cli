package cmd

import (
	"context"
	"sync"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/api"
	"github.com/listo-delivery/listo-cli/internal/config"
	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

// EnvAPIKey is the environment variable that supplies an API key (lst_test_...
// or lst_live_...) instead of a stored login.
const EnvAPIKey = "LISTO_API_KEY"

// Runtime is what a command needs, built once per run from the persistent flags
// and the environment. Get it with Run(cmd).
type Runtime struct {
	Printer *output.Printer
	Getenv  func(string) string
	URLs    api.URLs

	// Live is --live; Mode is config.Live or config.Test accordingly. Every
	// credential, badge and write follows Mode: test is the default.
	Live bool
	Mode config.Mode

	// LiveGiven is true when --live was on the command line (a live key without
	// it is refused rather than silently switching the world).
	LiveGiven bool

	once  sync.Once
	store *config.Store
	serr  error
}

// GrantSource builds the api.TokenSource for a stored GRANT (the login lane's
// refreshing token source). The login lane assigns it in its own init():
//
//	func init() { GrantSource = newGrantSource }
//
// The source must refresh under rt.Store().WithLock, re-reading the record after
// the lock is taken (internal/config's package comment). A key never needs it.
var GrantSource func(rt *Runtime, mode config.Mode) (api.TokenSource, error)

type execState struct {
	streams output.Streams
	getenv  func(string) string
	rt      *Runtime
}

var state execState

// Run returns the Runtime of the current command line. It is built on first use
// from the root's persistent flags, so it is the same value in every command.
func Run(_ *cobra.Command) *Runtime { return currentRuntime() }

func currentRuntime() *Runtime {
	if state.rt != nil {
		return state.rt
	}
	pf := rootCmd.PersistentFlags()
	jsonOut, _ := pf.GetBool("json")
	live, _ := pf.GetBool("live")
	yes, _ := pf.GetBool("yes")
	state.rt = &Runtime{
		Printer:   output.New(state.streams, output.Options{JSON: jsonOut, Live: live, Yes: yes}, state.getenv),
		Getenv:    state.getenv,
		URLs:      api.ResolveURLs(state.getenv),
		Live:      live,
		LiveGiven: pf.Changed("live"),
		Mode:      config.ModeFor(live),
	}
	return state.rt
}

// Store opens the credential store on first use. A failure is a ready-to-return
// Failure (exit 1).
func (r *Runtime) Store() (*config.Store, error) {
	r.once.Do(func() {
		r.store, r.serr = config.OpenDefault(r.Getenv)
	})
	if r.serr != nil {
		return nil, output.Fail("config.unavailable", i18n.Args{"detail": r.serr.Error()}).Wrap(r.serr)
	}
	return r.store, nil
}

// TokenSource resolves the credential of the selected mode, in this order:
//
//  1. LISTO_API_KEY: a key, mode read from its prefix, which must agree with
//     --live (a live key without --live, or a test key with it, is a usage error);
//  2. the stored record of the mode: a key is used as is, a grant goes through
//     GrantSource;
//  3. nothing: "Run listo login" (or "Run listo login --live").
func (r *Runtime) TokenSource() (api.TokenSource, error) {
	if key := r.Getenv(EnvAPIKey); key != "" {
		mode, ok := api.KeyMode(key)
		if !ok {
			return nil, output.Usage("auth.bad_key_prefix", i18n.Args{"env": EnvAPIKey}).Next("auth.next_key_prefix")
		}
		if config.Mode(mode) != r.Mode {
			if mode == "live" {
				return nil, output.Usage("auth.live_key_needs_flag").Next("auth.next_add_live")
			}
			return nil, output.Usage("auth.test_key_with_live").Next("auth.next_drop_live")
		}
		return api.StaticKey(key), nil
	}
	store, err := r.Store()
	if err != nil {
		return nil, err
	}
	rec, err := store.Load(r.Mode)
	if err != nil {
		return nil, output.Fail("config.unreadable", i18n.Args{"detail": err.Error()}).Wrap(err)
	}
	switch {
	case rec.Credential.IsKey():
		return api.StaticKey(rec.Credential.APIKey), nil
	case rec.Credential.IsGrant():
		if GrantSource == nil {
			return nil, output.Fail("auth.grant_unavailable").Next("auth.next_login")
		}
		return GrantSource(r, r.Mode)
	}
	if r.Mode == config.Live {
		return nil, output.Fail("auth.no_live_session").WithCode("no_session").Next("auth.next_login_live")
	}
	return nil, output.Fail("auth.no_session").WithCode("no_session").Next("auth.next_login")
}

// Client is an API client for the selected mode with its credential resolved.
func (r *Runtime) Client(_ context.Context) (*api.Client, error) {
	tokens, err := r.TokenSource()
	if err != nil {
		return nil, err
	}
	return api.New(api.Options{BaseURL: r.URLs.API, Tokens: tokens}), nil
}
