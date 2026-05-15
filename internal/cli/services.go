package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

func newServicesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Inspect multienv services",
	}
	cmd.AddCommand(newServicesListCmd())
	return cmd
}

func newServicesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all containers recognized as multienv services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServicesList(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runServicesList(ctx context.Context, out io.Writer) error {
	dc, err := docker.New()
	if err != nil {
		return err
	}
	defer dc.Close()

	containers, err := dc.ListContainers(ctx)
	if err != nil {
		return err
	}
	return writeServicesTable(out, service.FromContainers(containers))
}

func writeServicesTable(out io.Writer, services []service.Service) error {
	sort.Slice(services, func(i, j int) bool {
		if services[i].Project != services[j].Project {
			return services[i].Project < services[j].Project
		}
		return services[i].Name < services[j].Name
	})

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "PROJECT\tNAME\tSTATUS\tACCESSORIES"); err != nil {
		return err
	}
	for _, s := range services {
		project := orDash(s.Project)
		accessories := orDash(strings.Join(s.Accessories, ", "))
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", project, s.Name, s.Status, accessories); err != nil {
			return err
		}
	}
	return w.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
