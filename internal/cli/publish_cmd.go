package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/reconciler"
	"github.com/lowski/multienv/internal/state"
)

// newPublishSubcommand returns the framework-provided `publish`
// subcommand for accessories that declare a HostBinding. With no args
// it prints the current binding; with "off" it disables; with a number
// it sets the host port and runs reconcile to apply the change.
func newPublishSubcommand(acc accessory.Accessory, reg *accessory.Registry) *cobra.Command {
	hb := acc.HostBinding()
	return &cobra.Command{
		Use:   "publish [PORT|off]",
		Short: fmt.Sprintf("Get or set the %s host-port binding", acc.Name()),
		Long: fmt.Sprintf(`Get or set the host-port binding for the %s accessory.

With no argument, prints the current binding. Pass a port number to
bind 127.0.0.1:PORT, or "off" to disable host publication entirely.

Setting the binding triggers a full reconcile so the change takes
effect immediately; data volumes are preserved.

%s default: %d`, acc.Name(), hb.Description, hb.DefaultPort),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPublishSubcommand(cmd.Context(), cmd.OutOrStdout(), acc, reg, args)
		},
	}
}

func runPublishSubcommand(ctx context.Context, out io.Writer, acc accessory.Accessory, reg *accessory.Registry, args []string) error {
	hb := acc.HostBinding()
	if len(args) == 0 {
		return printCurrentBinding(out, acc, hb)
	}

	st, err := state.Load()
	if err != nil {
		fmt.Fprintf(out, "warning: could not read existing state (continuing): %v\n", err)
		st = state.Empty()
	}

	arg := args[0]
	var newPort uint16
	switch arg {
	case "off", "disable", "disabled", "none":
		newPort = 0
	default:
		n, err := strconv.ParseUint(arg, 10, 16)
		if err != nil {
			return fmt.Errorf("invalid argument %q: expected a port number or \"off\"", arg)
		}
		if n == 0 {
			return errors.New("port 0 is not valid; use \"off\" to disable host publication")
		}
		newPort = uint16(n)
	}

	st.SetAccessoryHostPort(acc.Name(), newPort)
	if err := state.Save(st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	if newPort == 0 {
		fmt.Fprintf(out, "%s host publication disabled. Running reconcile to apply...\n\n", acc.Name())
	} else {
		fmt.Fprintf(out, "%s host_port set to 127.0.0.1:%d. Running reconcile to apply...\n\n", acc.Name(), newPort)
	}

	dc, err := docker.New()
	if err != nil {
		return err
	}
	defer dc.Close()
	r := &reconciler.Reconciler{
		Docker:      dc,
		Accessories: reg,
		Out:         out,
		Color:       isTerminal(out),
	}
	return r.Reconcile(ctx)
}

func printCurrentBinding(out io.Writer, acc accessory.Accessory, hb *accessory.HostBindingSpec) error {
	port := hb.DefaultPort
	source := "default"
	st, err := state.Load()
	if err == nil {
		if v, ok := st.AccessoryHostPort(acc.Name()); ok {
			port = v
			source = "state file"
		}
	}
	if port == 0 {
		fmt.Fprintf(out, "%s: host publication is off (%s)\n", acc.Name(), source)
		return nil
	}
	fmt.Fprintf(out, "%s: 127.0.0.1:%d (%s)\n", acc.Name(), port, source)
	return nil
}
