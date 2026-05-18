package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/labels"
	"github.com/lowski/multienv/internal/service"
	"github.com/lowski/multienv/internal/state"
)

// newServicesSubcommand returns the framework-provided `services`
// subcommand attached to every accessory.
func newServicesSubcommand(acc accessory.Accessory) *cobra.Command {
	return &cobra.Command{
		Use:   "services",
		Short: fmt.Sprintf("List services that use the %s accessory", acc.Name()),
		Long: "List services that use this accessory.\n\n" +
			"If the multienv state file exists, the listing includes services whose\n" +
			"containers have been removed, with their last-seen status. Without the\n" +
			"state file, only services with current containers are listed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServicesSubcommand(cmd.Context(), cmd.OutOrStdout(), acc)
		},
	}
}

// servicesRow is one rendered row in the `services` table.
type servicesRow struct {
	Key        string
	Status     string // "running" | "stopped" | "missing" | "" (when no state file)
	Columns    []accessory.Column
	LastSeen   time.Time
	HasLastSeen bool
}

func runServicesSubcommand(ctx context.Context, out io.Writer, acc accessory.Accessory) error {
	dc, err := docker.New()
	if err != nil {
		return err
	}
	defer dc.Close()

	containers, err := dc.ListContainers(ctx)
	if err != nil {
		return err
	}

	// Build live requests keyed by displayName.
	live := map[string]accessory.ServiceRequest{}
	liveStatus := map[string]string{}
	for _, c := range containers {
		cfg := labels.ForAccessory(c.Labels, acc.Name())
		if cfg == nil {
			continue
		}
		s, _ := service.FromContainer(c)
		key := displayName(s)
		live[key] = accessory.ServiceRequest{Service: s, Config: cfg}
		if c.State == "running" {
			liveStatus[key] = "running"
		} else {
			liveStatus[key] = "stopped"
		}
	}

	// State file is optional. A read error here means we fall back to
	// live-only rendering — we don't fail the command.
	st, stErr := state.Load()
	useState := stErr == nil && st != nil && len(st.Services) > 0

	var rows []servicesRow
	if useState {
		seen := map[string]bool{}
		for _, u := range st.ServicesUsingAccessory(acc.Name()) {
			seen[u.Key] = true
			// Prefer live config if the service exists now; fall back to
			// the last-recorded config from state.
			cfg := u.Config
			if r, ok := live[u.Key]; ok {
				cfg = r.Config
			}
			req := accessory.ServiceRequest{
				Service: synthService(u.Key),
				Config:  cfg,
			}
			status, ok := liveStatus[u.Key]
			if !ok {
				status = "missing"
			}
			rows = append(rows, servicesRow{
				Key:         u.Key,
				Status:      status,
				Columns:     acc.ServicesColumns(req),
				LastSeen:    u.LastSeenAt,
				HasLastSeen: true,
			})
		}
		// Include any live services not yet in state (state was wiped,
		// reconcile hasn't run, etc.).
		for key, r := range live {
			if seen[key] {
				continue
			}
			rows = append(rows, servicesRow{
				Key:     key,
				Status:  liveStatus[key],
				Columns: acc.ServicesColumns(r),
			})
		}
	} else {
		for key, r := range live {
			rows = append(rows, servicesRow{
				Key:     key,
				Columns: acc.ServicesColumns(r),
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return writeAccessoryServicesTable(out, acc, rows, useState)
}

func writeAccessoryServicesTable(out io.Writer, acc accessory.Accessory, rows []servicesRow, withState bool) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	headers := []string{"PROJECT/SERVICE"}
	// Use any sample row to learn the accessory's column headers. If
	// the table is empty, ask the accessory directly with an empty
	// request — accessories must return stable headers regardless of
	// input.
	var sample accessory.ServiceRequest
	cols := acc.ServicesColumns(sample)
	for _, c := range cols {
		headers = append(headers, c.Header)
	}
	if withState {
		headers = append(headers, "STATUS", "LAST SEEN")
	}
	fmt.Fprintln(w, joinTabs(headers))

	if len(rows) == 0 {
		w.Flush()
		fmt.Fprintf(out, "\nNo services currently use the %s accessory.\n", acc.Name())
		return nil
	}

	for _, row := range rows {
		cells := []string{row.Key}
		// Pad/align: each row's column values map index-for-index to
		// the headers we sampled above.
		for i := range cols {
			if i < len(row.Columns) {
				cells = append(cells, valueOrDash(row.Columns[i].Value))
				continue
			}
			cells = append(cells, "-")
		}
		if withState {
			cells = append(cells, row.Status, lastSeenStr(row.LastSeen, row.HasLastSeen))
		}
		fmt.Fprintln(w, joinTabs(cells))
	}
	return w.Flush()
}

func joinTabs(cells []string) string {
	return strings.Join(cells, "\t")
}

func valueOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func lastSeenStr(t time.Time, has bool) string {
	if !has || t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// synthService reconstructs the minimum service.Service needed to
// render a row from a state-file entry. Key is "<project>/<service>"
// or just "<service>" for non-compose containers.
func synthService(key string) service.Service {
	if project, name, ok := strings.Cut(key, "/"); ok {
		return service.Service{Project: project, Name: name}
	}
	return service.Service{Name: key}
}

func displayName(s service.Service) string {
	if s.Project == "" {
		return s.Name
	}
	return s.Project + "/" + s.Name
}
