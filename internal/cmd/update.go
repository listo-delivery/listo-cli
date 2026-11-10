package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
	"github.com/listo-delivery/listo-cli/internal/update"
	"github.com/listo-delivery/listo-cli/internal/version"
)

// Hooks the tests replace; nothing else touches them.
var (
	executablePath = os.Executable
	updateNow      = time.Now
	updateHTTP     *http.Client // nil means the update package's own clients
	updateKey      []byte       // nil means the embedded release key
)

func init() {
	rootCmd.AddCommand(newUpdateCmd())
	// The once-a-day update notice runs from the ROOT's persistent pre-run, the
	// one hook every command already passes through after its flags are parsed
	// (the --json and --lang that decide whether to print). Commands must not
	// define a PersistentPreRun of their own, or they would switch this off.
	rootCmd.PersistentPreRun = func(c *cobra.Command, _ []string) { updateNotice(c) }
}

// noticeSkips are commands whose output is a script or that are the upgrade
// itself, so a notice would be noise or a contradiction.
var noticeSkips = map[string]bool{
	"update": true, "completion": true, "help": true,
	"__complete": true, "__completeNoDesc": true,
}

// updateNotice prints at most one stderr line, at most once a day, when a newer
// release exists. It is silent on every failure and never changes the exit code.
func updateNotice(c *cobra.Command) {
	rt := Run(c)
	p := rt.Printer
	if p.JSON || !p.ErrTTY || noticeSkips[c.Name()] || rt.Getenv(update.EnvNoNotifier) != "" {
		return
	}
	store, err := rt.Store()
	if err != nil {
		return
	}
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	tag := update.CheckForNotice(ctx, update.NoticeOptions{
		Dir:     store.Dir(),
		Now:     updateNow(),
		Current: version.Version,
		HTTP:    updateHTTP,
	})
	if tag == "" {
		return
	}
	exe, _ := executablePath()
	fmt.Fprintln(p.Err, noticeLine(update.DetectMethod(rt.Getenv, exe), tag))
}

func noticeLine(m update.Method, tag string) string {
	args := i18n.Args{"latest": update.Bare(tag), "current": version.Version}
	if cmd := m.UpgradeCommand(); cmd != "" {
		args["command"] = cmd
		return i18n.T("update.notice", args)
	}
	args["url"] = docsURL()
	return i18n.T("update.notice_unknown", args)
}

func docsURL() string {
	if i18n.Lang() == i18n.Spanish {
		return "https://docs.listo.delivery/es/guides/cli"
	}
	return update.DocsInstallURL
}

type updateResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	InstallMethod   string `json:"install_method"`
	Updated         bool   `json:"updated"`
}

func newUpdateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "update",
		Short:   "update.short",
		Long:    "update.long",
		Example: "update.example",
		Args:    output.Args(cobra.NoArgs),
		RunE:    runUpdate,
	}
	c.Flags().Bool("check", false, "update.flag_check")
	return c
}

func runUpdate(c *cobra.Command, _ []string) error {
	rt := Run(c)
	p := rt.Printer
	check, _ := c.Flags().GetBool("check")
	exe, _ := executablePath()
	method := update.DetectMethod(rt.Getenv, exe)
	res := updateResult{Current: version.Version, InstallMethod: string(method)}

	// A package manager owns its copy: name its command and stop, with no
	// network call, unless the person only asked whether a release exists.
	if method != update.MethodScript && !check {
		return reportNotScript(p, method, res)
	}

	client := updateHTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	tag, err := update.LatestTag(c.Context(), client, update.LatestURL)
	if err != nil {
		return output.Fail("update.check_failed", i18n.Args{"detail": err.Error()}).
			Next("update.next_network").Wrap(err)
	}
	res.Latest = update.Bare(tag)
	res.UpdateAvailable = update.Newer(version.Version, tag)

	switch {
	case !res.UpdateAvailable:
		return printUpdate(p, res, "update.already_latest", i18n.Args{"current": res.Current})
	case check:
		return printUpdate(p, res, "update.available", i18n.Args{"latest": res.Latest, "current": res.Current})
	case method != update.MethodScript:
		return reportNotScript(p, method, res)
	}

	p.Infof("update.downloading", i18n.Args{"latest": res.Latest})
	applier := &update.Applier{
		HTTP: updateHTTP, Key: updateKey,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Exe: exe,
	}
	if err := applier.Apply(c.Context(), tag); err != nil {
		return updateFailure(err)
	}
	res.Updated = true
	return printUpdate(p, res, "update.done", i18n.Args{"current": res.Current, "latest": res.Latest})
}

func printUpdate(p *output.Printer, res updateResult, key string, args i18n.Args) error {
	if p.JSON {
		return p.PrintJSON(res)
	}
	p.Infof(key, args)
	return nil
}

func reportNotScript(p *output.Printer, m update.Method, res updateResult) error {
	if p.JSON {
		return p.PrintJSON(res)
	}
	if cmd := m.UpgradeCommand(); cmd != "" {
		p.Infof("update.package_manager", i18n.Args{"manager": m.Manager(), "command": cmd})
		return nil
	}
	p.Infof("update.not_script", i18n.Args{"command": update.ReinstallCommand})
	return nil
}

func updateFailure(err error) error {
	switch {
	case errors.Is(err, update.ErrNoSigningKey):
		return output.Fail("update.no_signing_key").Next("update.next_reinstall", i18n.Args{"command": update.ReinstallCommand}).Wrap(err)
	case errors.Is(err, update.ErrBadSignature), errors.Is(err, update.ErrChecksum):
		return output.Fail("update.verify_failed", i18n.Args{"detail": err.Error()}).Next("update.next_retry").Wrap(err)
	case errors.Is(err, update.ErrNotWritable):
		return output.Fail("update.write_failed", i18n.Args{"detail": err.Error()}).Next("update.next_reinstall", i18n.Args{"command": update.ReinstallCommand}).Wrap(err)
	case errors.Is(err, update.ErrUnsupported):
		return output.Fail("update.unsupported_platform", i18n.Args{"os": runtime.GOOS, "arch": runtime.GOARCH}).Next("update.next_reinstall", i18n.Args{"command": update.ReinstallCommand}).Wrap(err)
	}
	return output.Fail("update.download_failed", i18n.Args{"detail": err.Error()}).Next("update.next_network").Wrap(err)
}
