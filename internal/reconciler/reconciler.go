// Package reconciler converges the actual state of the local Docker
// daemon toward the state multienv expects: a shared network exists and
// every multienv service is attached to it.
//
// Reconcile is safe to call repeatedly. Each call is a one-shot pass; a
// future daemon can drive it from Docker events without changes here.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

// NetworkName is the name of the shared network every multienv service
// is attached to.
const NetworkName = "multienv"

// ManagedLabel marks resources that multienv owns and manages.
const ManagedLabel = "multienv.managed"

// DockerAPI is the slice of [docker.Client] the reconciler relies on.
// It is kept narrow so tests can substitute a fake.
type DockerAPI interface {
	ListContainers(ctx context.Context) ([]docker.Container, error)
	NetworkInspect(ctx context.Context, name string) (*docker.Network, error)
	NetworkCreate(ctx context.Context, spec docker.NetworkSpec) (string, error)
	NetworkConnect(ctx context.Context, networkID, containerID string) error
}

// Reconciler runs a single reconciliation pass over the Docker daemon.
type Reconciler struct {
	Docker DockerAPI
	Out    io.Writer
	DryRun bool
	// Color enables ANSI color in the prefix tags. The CLI sets this
	// based on whether stdout is a TTY; tests leave it off.
	Color bool
}

// Reconcile runs every reconciliation step. Per-service failures are
// collected so one bad service does not block the others; the joined
// error is returned at the end.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	netID, attached, err := r.ensureNetwork(ctx)
	if err != nil {
		return err
	}
	return r.attachServices(ctx, netID, attached)
}

// ensureNetwork makes sure the shared multienv network exists. It
// returns the network ID and the set of container IDs already attached
// to it. In dry-run mode, when the network does not yet exist, it
// returns an empty ID and an empty attached set so the caller can still
// describe the work it would do.
func (r *Reconciler) ensureNetwork(ctx context.Context) (string, map[string]struct{}, error) {
	net, err := r.Docker.NetworkInspect(ctx, NetworkName)
	if err == nil {
		r.logHousekeeping("network %q already exists (id=%s)", NetworkName, shortID(net.ID))
		return net.ID, net.AttachedContainerIDs, nil
	}
	if !errors.Is(err, docker.ErrNotFound) {
		return "", nil, fmt.Errorf("inspect network: %w", err)
	}

	spec := docker.NetworkSpec{
		Name:       NetworkName,
		Driver:     "bridge",
		Attachable: true,
		Labels:     map[string]string{ManagedLabel: "true"},
	}
	if r.DryRun {
		r.logHousekeeping("would create network %q (driver=bridge, attachable=true)", NetworkName)
		return "", map[string]struct{}{}, nil
	}
	id, err := r.Docker.NetworkCreate(ctx, spec)
	if err != nil {
		return "", nil, fmt.Errorf("create network: %w", err)
	}
	r.logHousekeeping("created network %q (id=%s)", NetworkName, shortID(id))
	return id, map[string]struct{}{}, nil
}

// attachServices connects every multienv service that is not already
// attached to the shared network.
func (r *Reconciler) attachServices(ctx context.Context, netID string, attached map[string]struct{}) error {
	containers, err := r.Docker.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	services := service.FromContainers(containers)

	var errs []error
	for _, s := range services {
		if _, ok := attached[s.ContainerID]; ok {
			r.logService(s, "already attached to %q", NetworkName)
			continue
		}
		if r.DryRun {
			r.logService(s, "would attach to %q", NetworkName)
			continue
		}
		if err := r.Docker.NetworkConnect(ctx, netID, s.ContainerID); err != nil {
			r.logService(s, "FAILED to attach: %v", err)
			errs = append(errs, fmt.Errorf("%s: %w", displayName(s), err))
			continue
		}
		r.logService(s, "attached to %q", NetworkName)
	}
	if len(errs) > 0 {
		return fmt.Errorf("attach services: %w", errors.Join(errs...))
	}
	return nil
}

// --- log channels ---------------------------------------------------------

// ANSI colors are kept simple; they apply only to the bracket-prefix tag.
// Future accessory channel: magenta (35).
const (
	ansiReset      = "\x1b[0m"
	colorHousekeep = "\x1b[36m" // cyan
	colorService   = "\x1b[32m" // green
)

func (r *Reconciler) logHousekeeping(format string, args ...any) {
	r.writeLine(colorHousekeep, "[housekeeping]", format, args...)
}

func (r *Reconciler) logService(s service.Service, format string, args ...any) {
	r.writeLine(colorService, fmt.Sprintf("[service %s]", displayName(s)), format, args...)
}

func (r *Reconciler) writeLine(color, prefix, format string, args ...any) {
	if r.Out == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if r.Color {
		fmt.Fprintf(r.Out, "%s%s%s %s\n", color, prefix, ansiReset, msg)
		return
	}
	fmt.Fprintf(r.Out, "%s %s\n", prefix, msg)
}

func displayName(s service.Service) string {
	if s.Project == "" {
		return s.Name
	}
	return s.Project + "/" + s.Name
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
