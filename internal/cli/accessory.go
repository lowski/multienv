package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
)

// reservedAccessoryCommands are subcommand names provided by the
// framework. An accessory cannot define commands with these names.
var reservedAccessoryCommands = map[string]bool{
	"services": true,
	"publish":  true,
}

// addAccessoryCommands registers one top-level command per accessory.
// The framework attaches `services` to every accessory and `publish`
// to those that declare a HostBinding; each accessory contributes its
// own Commands() for accessory-specific actions.
func addAccessoryCommands(root *cobra.Command, reg *accessory.Registry) {
	for _, acc := range reg.All() {
		ownCommands := acc.Commands()
		for _, c := range ownCommands {
			if reservedAccessoryCommands[c.Name] {
				panic(fmt.Sprintf(
					"accessory %q tried to register reserved command %q; reserved names are provided by the framework",
					acc.Name(), c.Name,
				))
			}
		}

		parent := &cobra.Command{
			Use:   acc.Name(),
			Short: fmt.Sprintf("Manage the %s accessory", acc.Name()),
			Long:  renderAccessoryHelp(acc),
		}

		parent.AddCommand(newServicesSubcommand(acc))
		if acc.HostBinding() != nil {
			parent.AddCommand(newPublishSubcommand(acc, reg))
		}
		for _, sub := range ownCommands {
			parent.AddCommand(toCobra(acc, sub))
		}

		root.AddCommand(parent)
	}
}

// renderAccessoryHelp builds a human-readable help body from an
// accessory's ConfigSchema.
func renderAccessoryHelp(acc accessory.Accessory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Manage the %s accessory.\n", acc.Name())

	schema := acc.ConfigSchema()
	if len(schema) > 0 {
		b.WriteString("\nConfiguration labels (set on the service container):\n")
		keys := make([]string, 0, len(schema))
		for k := range schema {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			opt := schema[k]
			req := ""
			if opt.Required {
				req = " (required)"
			}
			fmt.Fprintf(&b, "\n  multienv.%s.%s%s\n    %s\n", acc.Name(), k, req, opt.Description)
			if opt.Default != "" {
				fmt.Fprintf(&b, "    Default: %s\n", opt.Default)
			}
		}
	}
	return b.String()
}

// toCobra wraps a framework-free accessory.Command into a cobra.Command
// bound to the accessory's logging channel.
func toCobra(acc accessory.Accessory, c accessory.Command) *cobra.Command {
	return &cobra.Command{
		Use:   c.Name,
		Short: c.Short,
		Long:  c.Long,
		RunE: func(cmd *cobra.Command, args []string) error {
			dc, err := docker.New()
			if err != nil {
				return err
			}
			defer dc.Close()
			out := cmd.OutOrStdout()
			env := accessory.Env{
				Docker:  dc,
				Network: "multienv",
				Log: func(format string, a ...any) {
					fmt.Fprintf(out, "[accessory %s] ", acc.Name())
					fmt.Fprintf(out, format+"\n", a...)
				},
			}
			return c.Run(cmd.Context(), env, args)
		},
	}
}
