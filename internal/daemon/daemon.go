// Package daemon drives the reconciler from Docker events. It runs an
// initial reconcile, then subscribes to the daemon's event stream and
// triggers a debounced reconcile whenever something relevant changes.
package daemon

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/lowski/multienv/internal/docker"
)

// DefaultDebounce is the default quiet window before a reconcile fires.
// It coalesces the burst of events emitted by `docker compose up`.
const DefaultDebounce = 500 * time.Millisecond

// resubscribeBackoff is how long the daemon waits before reopening the
// event stream after a transient error.
const resubscribeBackoff = 2 * time.Second

// EventSource is the slice of [docker.Client] the daemon depends on.
// Defined here so tests can stub it without touching Docker.
type EventSource interface {
	Events(ctx context.Context) (<-chan docker.Event, <-chan error)
}

// Daemon watches the Docker daemon and reconciles in response to events.
type Daemon struct {
	Events    EventSource
	Reconcile func(ctx context.Context) error
	Out       io.Writer
	Color     bool
	Debounce  time.Duration
}

// Run blocks until ctx is cancelled. Reconcile failures are logged and
// the loop continues; the daemon only exits when ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	if d.Debounce <= 0 {
		d.Debounce = DefaultDebounce
	}
	d.log("daemon started (debounce=%s)", d.Debounce)
	defer d.log("daemon stopped")

	d.fire(ctx, "startup")

	for ctx.Err() == nil {
		if err := d.watch(ctx); err != nil {
			if ctx.Err() != nil {
				break
			}
			d.log("event stream error: %v (resubscribing in %s)", err, resubscribeBackoff)
			select {
			case <-time.After(resubscribeBackoff):
			case <-ctx.Done():
				return nil
			}
		}
	}
	return nil
}

// watch opens one event subscription and processes events until the
// stream errors or ctx is cancelled. The debouncer accumulates events
// and triggers one reconcile per quiet window.
func (d *Daemon) watch(ctx context.Context) error {
	events, errs := d.Events.Events(ctx)

	var (
		timer   *time.Timer
		fireC   <-chan time.Time
		reason  string
		pending int
	)
	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stopTimer()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-errs:
			if !ok || err == nil {
				return nil
			}
			return err
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if !relevant(ev) {
				continue
			}
			reason = describe(ev)
			pending++
			if timer == nil {
				timer = time.NewTimer(d.Debounce)
				fireC = timer.C
			} else {
				stopTimer()
				timer.Reset(d.Debounce)
			}
		case <-fireC:
			timer = nil
			fireC = nil
			r := reason
			if pending > 1 {
				r = fmt.Sprintf("%s (+%d more)", r, pending-1)
			}
			reason = ""
			pending = 0
			d.fire(ctx, r)
		}
	}
}

// fire runs one reconcile and reports outcome. Reconcile == nil
// short-circuits — useful for tests that only want to observe
// scheduling behavior.
func (d *Daemon) fire(ctx context.Context, reason string) {
	d.log("reconciling: %s", reason)
	if d.Reconcile == nil {
		return
	}
	if err := d.Reconcile(ctx); err != nil {
		d.log("reconcile failed: %v", err)
	}
}

// relevant filters event-stream noise down to the actions that can
// change what the reconciler should do.
func relevant(e docker.Event) bool {
	switch e.Kind {
	case docker.EventKindContainer:
		switch e.Action {
		case "create", "start", "die", "destroy", "kill", "rename":
			return true
		}
	case docker.EventKindNetwork:
		switch e.Action {
		case "create", "destroy", "connect", "disconnect":
			return true
		}
	}
	return false
}

// describe builds a short human-readable reason string for a single
// event. Used as the trigger annotation on the reconcile log line.
func describe(e docker.Event) string {
	id := e.ActorID
	if len(id) > 12 {
		id = id[:12]
	}
	name := e.Attributes["name"]
	switch e.Kind {
	case docker.EventKindContainer:
		if name != "" {
			return fmt.Sprintf("container %s %s", e.Action, name)
		}
		return fmt.Sprintf("container %s %s", e.Action, id)
	case docker.EventKindNetwork:
		if name != "" {
			return fmt.Sprintf("network %s %s", e.Action, name)
		}
		return fmt.Sprintf("network %s %s", e.Action, id)
	}
	return fmt.Sprintf("%s %s", e.Kind, e.Action)
}

// --- log ------------------------------------------------------------------

const (
	ansiReset      = "\x1b[0m"
	colorHousekeep = "\x1b[36m"
)

func (d *Daemon) log(format string, args ...any) {
	if d.Out == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	if d.Color {
		fmt.Fprintf(d.Out, "%s[housekeeping]%s %s\n", colorHousekeep, ansiReset, msg)
		return
	}
	fmt.Fprintf(d.Out, "[housekeeping] %s\n", msg)
}
