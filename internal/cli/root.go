// Package cli wires up the multienv command tree.
package cli

import "github.com/spf13/cobra"

// NewRoot returns the root multienv command with all subcommands attached.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "multienv",
		Short:         "Manage dev containers and shared accessories",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newServicesCmd())
	return root
}
