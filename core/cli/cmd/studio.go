package cmd

import (
	"github.com/spf13/cobra"
	"github.com/test-mesh/core/core/cli/cmd/studiolocal"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "studio [workspace]",
		Short: "Start the local Studio YAML workbench",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workspace := "."
			if len(args) == 1 {
				workspace = args[0]
			}
			return studiolocal.Serve(cmd.Context(), workspace, cmd.OutOrStdout())
		},
	})
}
