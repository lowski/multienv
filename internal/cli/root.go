// Package cli wires up the multienv command tree.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/accessory/postgres"
	"github.com/lowski/multienv/internal/accessory/proxy"
	"github.com/lowski/multienv/internal/accessory/s3"
)

// NewRoot returns the root multienv command with all subcommands attached.
func NewRoot() *cobra.Command {
	reg := buildRegistry()

	root := &cobra.Command{
		Use:           "multienv",
		Short:         "Manage dev containers and shared accessories",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(newServicesCmd())
	root.AddCommand(newReconcileCmd(reg))
	addAccessoryCommands(root, reg)
	return root
}

// buildRegistry constructs the accessory.Registry with every accessory
// multienv ships.
func buildRegistry() *accessory.Registry {
	reg := accessory.NewRegistry()
	reg.Register(proxy.New())
	reg.Register(postgres.New())
	reg.Register(s3.New())
	return reg
}
