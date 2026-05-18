// Package reconciler converges the actual state of the local Docker
// daemon toward the state multienv expects: a shared network exists,
// every multienv service is attached to it, and every registered
// accessory is reconciled against the services that request it.
//
// Reconcile is safe to call repeatedly. Each call is a one-shot pass; a
// future daemon can drive it from Docker events without changes here.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/labels"
	"github.com/lowski/multienv/internal/service"
	"github.com/lowski/multienv/internal/state"
)

// NetworkName is the name of the shared network every multienv service
// is attached to.
const NetworkName = "multienv"

// ManagedLabel marks resources that multienv owns and manages.
const ManagedLabel = "multienv.managed"

// DockerAPI is the slice of [docker.Client] the reconciler relies on.
// It mirrors [accessory.DockerAPI] so accessories and the reconciler
// share one fake in tests.
type DockerAPI = accessory.DockerAPI

// Reconciler runs a single reconciliation pass over the Docker daemon.
type Reconciler struct {
	Docker      DockerAPI
	Accessories *accessory.Registry
	Out         io.Writer
	DryRun      bool
	// Color enables ANSI color in the prefix tags. The CLI sets this
	// based on whether stdout is a TTY; tests leave it off.
	Color bool
}

// Reconcile runs every reconciliation step. Per-step failures are
// collected so one bad step does not block the others.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	st, err := state.Load()
	if err != nil {
		r.logHousekeeping("could not read state file (using defaults): %v", err)
	}

	netID, attached, err := r.ensureNetwork(ctx)
	if err != nil {
		return err
	}
	containers, err := r.Docker.ListContainers(ctx)
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	services := service.FromContainers(containers)

	var errs []error
	if err := r.attachServices(ctx, netID, attached, services); err != nil {
		errs = append(errs, err)
	}
	if err := r.reconcileAccessories(ctx, netID, containers, services, st); err != nil {
		errs = append(errs, err)
	}

	if !r.DryRun {
		if err := state.Save(st); err != nil {
			r.logHousekeeping("could not persist state: %v", err)
		}
	}
	return errors.Join(errs...)
}

// ensureNetwork makes sure the shared multienv network exists.
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
func (r *Reconciler) attachServices(ctx context.Context, netID string, attached map[string]struct{}, services []service.Service) error {
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

// reconcileAccessories drives each registered accessory and records the
// observed service-to-accessory mapping into state.
func (r *Reconciler) reconcileAccessories(ctx context.Context, netID string, containers []docker.Container, services []service.Service, st *state.State) error {
	if r.Accessories == nil {
		return nil
	}
	labelsByID := indexLabels(containers)

	// Per-service map accumulated across all accessories so the state
	// file ends up with one entry per service holding every accessory
	// it was observed to use this round.
	perService := map[string]map[string]map[string]string{}

	var errs []error
	for _, acc := range r.Accessories.All() {
		reqs := r.collectRequests(acc.Name(), services, labelsByID)

		for _, req := range reqs {
			key := displayName(req.Service)
			if perService[key] == nil {
				perService[key] = map[string]map[string]string{}
			}
			perService[key][acc.Name()] = req.Config
		}

		env := r.accessoryEnv(acc, netID, st)
		if err := acc.Reconcile(ctx, env, reqs); err != nil {
			r.logAccessory(acc.Name(), "FAILED: %v", err)
			errs = append(errs, fmt.Errorf("%s: %w", acc.Name(), err))
		}
	}

	for key, cfgs := range perService {
		st.RecordService(key, cfgs)
	}

	if len(errs) > 0 {
		return fmt.Errorf("reconcile accessories: %w", errors.Join(errs...))
	}
	return nil
}

func (r *Reconciler) collectRequests(name string, services []service.Service, labelsByID map[string]map[string]string) []accessory.ServiceRequest {
	var out []accessory.ServiceRequest
	for _, s := range services {
		cfg := labels.ForAccessory(labelsByID[s.ContainerID], name)
		if cfg == nil {
			continue
		}
		out = append(out, accessory.ServiceRequest{Service: s, Config: cfg})
	}
	return out
}

// accessoryEnv builds the per-accessory execution context, resolving
// the host-bound port from state (or the accessory's default).
func (r *Reconciler) accessoryEnv(acc accessory.Accessory, netID string, st *state.State) accessory.Env {
	var hostPort uint16
	if hb := acc.HostBinding(); hb != nil {
		hostPort = hb.DefaultPort
		if v, ok := st.AccessoryHostPort(acc.Name()); ok {
			hostPort = v
		}
	}
	name := acc.Name()
	return accessory.Env{
		Docker:        r.Docker,
		NetworkID:     netID,
		Network:       NetworkName,
		DryRun:        r.DryRun,
		HostBoundPort: hostPort,
		Log: func(format string, args ...any) {
			r.logAccessory(name, format, args...)
		},
	}
}

func indexLabels(containers []docker.Container) map[string]map[string]string {
	out := make(map[string]map[string]string, len(containers))
	for _, c := range containers {
		out[c.ID] = c.Labels
	}
	return out
}

// --- log channels ---------------------------------------------------------

const (
	ansiReset      = "\x1b[0m"
	colorHousekeep = "\x1b[36m" // cyan
	colorService   = "\x1b[32m" // green
	colorAccessory = "\x1b[35m" // magenta
)

func (r *Reconciler) logHousekeeping(format string, args ...any) {
	r.writeLine(colorHousekeep, "[housekeeping]", format, args...)
}

func (r *Reconciler) logService(s service.Service, format string, args ...any) {
	r.writeLine(colorService, fmt.Sprintf("[service %s]", displayName(s)), format, args...)
}

func (r *Reconciler) logAccessory(name, format string, args ...any) {
	r.writeLine(colorAccessory, fmt.Sprintf("[accessory %s]", name), format, args...)
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
