package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
)

// addAccessoryCommands registers one top-level command per accessory.
// The parent command's help text is rendered from ConfigSchema(); each
// accessory.Command is wrapped into a cobra.Command so accessories
// never import cobra themselves.
func addAccessoryCommands(root *cobra.Command, reg *accessory.Registry) {
	for _, acc := range reg.All() {
		parent := &cobra.Command{
			Use:   acc.Name(),
			Short: fmt.Sprintf("Manage the %s accessory", acc.Name()),
			Long:  renderAccessoryHelp(acc),
		}
		for _, sub := range acc.Commands() {
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
