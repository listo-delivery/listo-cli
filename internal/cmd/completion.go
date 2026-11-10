package cmd

import (
	"github.com/spf13/cobra"

	"github.com/listo-delivery/listo-cli/internal/i18n"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func init() { rootCmd.AddCommand(newCompletionCmd()) }

var shells = []string{"bash", "zsh", "fish", "powershell"}

func newCompletionCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "completion bash|zsh|fish|powershell",
		Short:     "completion.short",
		Long:      "completion.long",
		Example:   "completion.example",
		ValidArgs: shells,
		Args:      output.Args(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, args []string) error {
			out := c.OutOrStdout()
			switch args[0] {
			case "bash":
				return c.Root().GenBashCompletionV2(out, true)
			case "zsh":
				return c.Root().GenZshCompletion(out)
			case "fish":
				return c.Root().GenFishCompletion(out, true)
			case "powershell":
				return c.Root().GenPowerShellCompletionWithDesc(out)
			}
			return output.Usage("completion.unknown_shell", i18n.Args{"shell": args[0]}).
				Next("completion.next_shells")
		},
	}
}
