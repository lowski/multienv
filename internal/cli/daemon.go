package cli

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/daemon"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/reconciler"
)

func newDaemonCmd(reg *accessory.Registry) *cobra.Command {
	var debounce time.Duration
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Watch Docker events and reconcile continuously",
		Long: `Run multienv as a foreground daemon.

Performs an initial reconcile, then subscribes to the Docker event stream
and reconciles whenever a container or network change could affect the
multienv environment. Events are debounced so a burst from "docker compose up"
collapses into a single reconcile pass.

Press Ctrl-C to stop.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dc, err := docker.New()
			if err != nil {
				return err
			}
			defer dc.Close()

			out := cmd.OutOrStdout()
			color := isTerminal(out)
			r := &reconciler.Reconciler{
				Docker:      dc,
				Accessories: reg,
				Out:         out,
				Color:       color,
			}
			d := &daemon.Daemon{
				Events:    dc,
				Reconcile: r.Reconcile,
				Out:       out,
				Color:     color,
				Debounce:  debounce,
			}
			return d.Run(cmd.Context())
		},
	}
	cmd.Flags().DurationVar(&debounce, "debounce", daemon.DefaultDebounce,
		"quiet window after the last event before a reconcile fires")
	return cmd
}
