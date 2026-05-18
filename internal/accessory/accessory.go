// Package accessory defines the shared interface and runtime support
// for multienv's centrally-managed accessory containers (proxy,
// database, object store, ...).
//
// An accessory implementation lives in its own subpackage (e.g.
// internal/accessory/proxy) and is responsible for both its container
// lifecycle and the per-service configuration declared via labels.
package accessory

import (
	"context"
	"io"
	"sort"

	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

// Accessory is the interface every accessory implementation satisfies.
//
// Name must match the label segment used by services to opt in. For
// example, accessory "proxy" reacts to labels like
// "multienv.proxy.domain=app.example.com".
type Accessory interface {
	Name() string
	ConfigSchema() map[string]ConfigOption
	Commands() []Command
	Reconcile(ctx context.Context, env Env, requests []ServiceRequest) error

	// HostBinding describes the accessory's host-port binding, or nil if
	// the accessory does not expose a configurable host port. Returning
	// non-nil enables the framework-provided `publish` subcommand.
	HostBinding() *HostBindingSpec

	// ServicesColumns returns the per-service columns rendered by the
	// framework-provided `services` subcommand, after the common
	// PROJECT/SERVICE columns. Each accessory pulls the values from the
	// request's labels.
	ServicesColumns(req ServiceRequest) []Column
}

// HostBindingSpec declares an accessory's host-port binding shape. The
// state file is consulted for the actual port at runtime; this struct
// only provides the default and human description for help output.
type HostBindingSpec struct {
	Description string // shown in `publish` help, e.g. "PostgreSQL"
	DefaultPort uint16 // initial port when nothing recorded in state
}

// Column is one cell in a `services` table row.
type Column struct {
	Header string
	Value  string
}

// ConfigOption documents one label-driven configuration option.
type ConfigOption struct {
	Description string
	Required    bool
	Default     string // human-readable default; empty if none
}

// Command is a framework-free description of an accessory subcommand.
// The cli package wraps these into cobra.Command at registration time
// so accessories do not import cobra.
type Command struct {
	Name  string
	Short string
	Long  string
	Run   func(ctx context.Context, env Env, args []string) error
}

// ServiceRequest is one service's declared interest in an accessory.
// Config holds the labels under "multienv.<accessory>." with the prefix
// stripped — e.g. for proxy, key "domain" → "app.example.com".
type ServiceRequest struct {
	Service service.Service
	Config  map[string]string
}

// DockerAPI is the subset of docker.Client an accessory may use. It is
// kept on this package so the reconciler and accessories share one
// interface; tests can substitute a fake.
type DockerAPI interface {
	ListContainers(ctx context.Context) ([]docker.Container, error)
	NetworkInspect(ctx context.Context, name string) (*docker.Network, error)
	NetworkCreate(ctx context.Context, spec docker.NetworkSpec) (string, error)
	NetworkConnect(ctx context.Context, networkID, containerID string) error
	ContainerInspect(ctx context.Context, nameOrID string) (*docker.Container, error)
	ContainerCreate(ctx context.Context, spec docker.ContainerSpec) (string, error)
	ContainerStart(ctx context.Context, id string) error
	ContainerRemove(ctx context.Context, id string, force bool) error
	ContainerExec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout io.Writer) error
	ContainerCopyFrom(ctx context.Context, id, srcPath string, dst io.Writer) error
	ImagePull(ctx context.Context, ref string) error
}

// Env is the per-call execution context handed to an accessory. The
// reconciler constructs a fresh Env for each Reconcile call, with Log
// pre-bound to emit "[accessory <name>] ..." through the chosen
// channel.
type Env struct {
	Docker    DockerAPI
	NetworkID string // ID of the shared multienv network ("" if unknown)
	Network   string // name of the shared multienv network
	Log       func(format string, args ...any)
	DryRun    bool
	// HostBoundPort is the resolved host port the accessory should
	// publish on, or 0 if the host binding is disabled / not applicable.
	// The reconciler computes this from the state file, falling back to
	// the accessory's HostBindingSpec.DefaultPort.
	HostBoundPort uint16
}

// Registry collects accessory implementations for the CLI and reconciler.
type Registry struct {
	items map[string]Accessory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{items: map[string]Accessory{}}
}

// Register adds an accessory. It panics on duplicate names because the
// registry is built once at startup with literal values.
func (r *Registry) Register(a Accessory) {
	if _, exists := r.items[a.Name()]; exists {
		panic("accessory already registered: " + a.Name())
	}
	r.items[a.Name()] = a
}

// Get returns an accessory by name.
func (r *Registry) Get(name string) (Accessory, bool) {
	a, ok := r.items[name]
	return a, ok
}

// All returns the registered accessories, sorted by name for stable
// iteration in the reconciler and the CLI.
func (r *Registry) All() []Accessory {
	out := make([]Accessory, 0, len(r.items))
	for _, a := range r.items {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
