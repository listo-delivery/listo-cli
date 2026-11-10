package cmd

import (
	"runtime"

	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
	"github.com/listo-delivery/listo-cli/internal/version"
)

func init() { rootCmd.AddCommand(newVersionCmd()) }

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		Short:   "version.short",
		Example: "version.example",
		Args:    output.Args(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			p := Run(c).Printer
			if p.JSON {
				return p.PrintJSON(map[string]string{
					"version": version.Version,
					"os":      runtime.GOOS,
					"arch":    runtime.GOARCH,
				})
			}
			p.Infof("version.line", i18n.Args{"version": version.Version, "os": runtime.GOOS, "arch": runtime.GOARCH})
			return nil
		},
	}
}
