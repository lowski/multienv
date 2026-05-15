package cli

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/reconciler"
)

func newReconcileCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile the local Docker state to match multienv expectations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReconcile(cmd.Context(), cmd.OutOrStdout(), dryRun)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "describe planned actions without performing them")
	return cmd
}

func runReconcile(ctx context.Context, out io.Writer, dryRun bool) error {
	dc, err := docker.New()
	if err != nil {
		return err
	}
	defer dc.Close()

	r := &reconciler.Reconciler{
		Docker: dc,
		Out:    out,
		DryRun: dryRun,
		Color:  isTerminal(out),
	}
	return r.Reconcile(ctx)
}

// isTerminal reports whether w writes to an interactive terminal.
// Honors the NO_COLOR convention (https://no-color.org/).
func isTerminal(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
