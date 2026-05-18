// Package proxy implements multienv's HTTPS reverse-proxy accessory,
// backed by Caddy. Services opt in by declaring multienv.proxy.domain
// (and optionally multienv.proxy.port); this package owns the
// multienv-proxy container's lifecycle and pushes route configuration
// into it via Caddy's admin API (over docker exec).
package proxy

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

const (
	// ContainerName is the fixed name of the proxy container.
	ContainerName = "multienv-proxy"
	// Image is the Caddy image multienv runs.
	Image = "caddy:2-alpine"
	// VolumeName is the Docker volume that persists Caddy's data
	// directory (internal CA, issued certs, autosave.json).
	VolumeName = "multienv-proxy-data"
	// caddyDataPath is where the volume is mounted inside the container.
	caddyDataPath = "/data"
	// caRootPath is where Caddy's internal CA root cert lives once
	// Caddy has provisioned at least one local cert.
	caRootPath = "/data/caddy/pki/authorities/local/root.crt"
)

// Accessory is the proxy accessory implementation.
type Accessory struct{}

// New returns a ready-to-register proxy accessory.
func New() *Accessory { return &Accessory{} }

func (Accessory) Name() string { return "proxy" }

func (Accessory) ConfigSchema() map[string]accessory.ConfigOption {
	return map[string]accessory.ConfigOption{
		"domain": {
			Description: "The public-facing hostname the proxy should serve, e.g. app.example.com. " +
				"Accepts a comma-separated list to route multiple domains to the same upstream.",
			Required: true,
		},
		"port": {
			Description: "Target port on the service container.",
			Required:    false,
			Default:     "first exposed port of the container",
		},
	}
}

// HostBinding returns nil — the proxy's host bindings (80 and 443) are
// not user-configurable: they are the whole point of running the proxy.
func (Accessory) HostBinding() *accessory.HostBindingSpec { return nil }

// ServicesColumns renders the proxy's per-service columns for the
// framework `services` listing.
func (Accessory) ServicesColumns(r accessory.ServiceRequest) []accessory.Column {
	return []accessory.Column{
		{Header: "DOMAIN", Value: r.Config["domain"]},
		{Header: "PORT", Value: r.Config["port"]},
	}
}

func (a Accessory) Commands() []accessory.Command {
	return []accessory.Command{
		{
			Name:  "trust-ca",
			Short: "Extract the Caddy local-CA root cert and print trust instructions",
			Long: "Extracts Caddy's internal root certificate from the proxy container " +
				"to a temp file and prints the OS-appropriate command to add it to the " +
				"system trust store. Requires the proxy to have been started at least once.",
			Run: a.trustCA,
		},
	}
}

// Reconcile ensures the proxy container is running (when there are
// routable services) and that its loaded routes match the requests.
func (Accessory) Reconcile(ctx context.Context, env accessory.Env, requests []accessory.ServiceRequest) error {
	if len(requests) == 0 {
		env.Log("no services request the proxy; nothing to do")
		return nil
	}

	routes, perServiceErrs := buildRoutes(requests)
	for name, err := range perServiceErrs {
		env.Log("skipping %s: %v", name, err)
	}
	if len(routes) == 0 {
		env.Log("no routable services; not starting the proxy container")
		return nil
	}

	if env.DryRun {
		env.Log("would ensure container %q (image=%s, network=%s)", ContainerName, Image, env.Network)
		for _, r := range routes {
			env.Log("would route %s → %s", r.Domain, r.Upstream)
		}
		return nil
	}

	id, err := ensureContainer(ctx, env)
	if err != nil {
		return err
	}

	caddyfile := generateCaddyfile(routes)
	if err := pushConfig(ctx, env, id, caddyfile); err != nil {
		return fmt.Errorf("push config: %w", err)
	}
	for _, r := range routes {
		env.Log("routing %s → %s", r.Domain, r.Upstream)
	}
	return nil
}

// route is the proxy's internal representation of one service's
// requested HTTPS endpoint.
type route struct {
	Domain   string
	Upstream string // host:port reachable on the multienv network
}

// buildRoutes converts ServiceRequests into routes and reports any
// per-service problems so the caller can log them without failing the
// whole reconcile.
func buildRoutes(reqs []accessory.ServiceRequest) ([]route, map[string]error) {
	errs := map[string]error{}
	var routes []route
	for _, r := range reqs {
		name := displayName(r.Service)
		domains := splitDomains(r.Config["domain"])
		if len(domains) == 0 {
			errs[name] = errors.New("missing multienv.proxy.domain")
			continue
		}
		if r.Service.ContainerName == "" {
			errs[name] = errors.New("container has no name to route to")
			continue
		}
		port, err := resolvePort(r)
		if err != nil {
			errs[name] = err
			continue
		}
		upstream := fmt.Sprintf("%s:%d", r.Service.ContainerName, port)
		for _, d := range domains {
			routes = append(routes, route{Domain: d, Upstream: upstream})
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Domain < routes[j].Domain })
	return routes, errs
}

// splitDomains parses the multienv.proxy.domain label, which may be a
// single domain or a comma-separated list. Whitespace is trimmed and
// empty entries are dropped.
func splitDomains(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if d := strings.TrimSpace(p); d != "" {
			out = append(out, d)
		}
	}
	return out
}

// ensureContainer returns the running proxy container's ID, creating
// and starting it if necessary.
func ensureContainer(ctx context.Context, env accessory.Env) (string, error) {
	c, err := env.Docker.ContainerInspect(ctx, ContainerName)
	if err == nil {
		if c.State != "running" {
			if err := env.Docker.ContainerStart(ctx, c.ID); err != nil {
				return "", fmt.Errorf("start existing proxy: %w", err)
			}
			env.Log("started existing container %q", ContainerName)
		}
		return c.ID, nil
	}
	if !errors.Is(err, docker.ErrNotFound) {
		return "", fmt.Errorf("inspect proxy: %w", err)
	}

	env.Log("pulling image %s", Image)
	if err := env.Docker.ImagePull(ctx, Image); err != nil {
		return "", err
	}

	spec := docker.ContainerSpec{
		Name:  ContainerName,
		Image: Image,
		Cmd:   []string{"caddy", "run", "--resume"},
		Labels: map[string]string{
			"multienv.managed":   "true",
			"multienv.accessory": "proxy",
		},
		Networks: []string{env.Network},
		HostBindings: []docker.PortBinding{
			{HostIP: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"},
			{HostIP: "0.0.0.0", HostPort: 443, ContainerPort: 443, Protocol: "tcp"},
		},
		Mounts: []docker.Mount{
			{Type: "volume", Source: VolumeName, Target: caddyDataPath},
		},
		RestartPolicy: "unless-stopped",
	}
	id, err := env.Docker.ContainerCreate(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := env.Docker.ContainerStart(ctx, id); err != nil {
		return "", err
	}
	env.Log("created container %q (image=%s, volume=%s)", ContainerName, Image, VolumeName)
	return id, nil
}

// pushConfig pipes the Caddyfile into the container, adapts it to JSON
// and loads it via Caddy's admin API. No host file involved.
func pushConfig(ctx context.Context, env accessory.Env, id, caddyfile string) error {
	const script = "cat > /tmp/multienv.Caddyfile && " +
		"caddy reload --adapter caddyfile --config /tmp/multienv.Caddyfile"
	return env.Docker.ContainerExec(ctx, id,
		[]string{"sh", "-c", script},
		strings.NewReader(caddyfile),
		nil,
	)
}

func displayName(s service.Service) string {
	if s.Project == "" {
		return s.Name
	}
	return s.Project + "/" + s.Name
}
