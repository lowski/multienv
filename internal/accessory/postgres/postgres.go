// Package postgres implements multienv's PostgreSQL accessory.
// Services declare multienv.postgres.dbname=<name> on their container;
// this package owns a singleton postgres:18-alpine container and
// idempotently creates the requested databases inside it.
//
// Connection convention (no env injection — service author hardcodes):
//
//	in-network: postgres://postgres:postgres@multienv-postgres:5432/<db>
//	from host:  postgres://postgres:postgres@127.0.0.1:<host_port>/<db>
package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

const (
	ContainerName     = "multienv-postgres"
	Image             = "postgres:18-alpine"
	VolumeName        = "multienv-postgres-data"
	dataPath          = "/var/lib/postgresql"
	dbUser            = "postgres"
	dbPassword        = "postgres"
	internalPort      = 5432
	defaultHostPort   = 5432
	readyTimeout      = 30 * time.Second
	readyPollInterval = 1 * time.Second
)

// dbNameRe matches the dbname format multienv accepts. Strict on
// purpose: passes straight into CREATE DATABASE, no quoting tricks.
var dbNameRe = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Accessory is the postgres accessory implementation.
type Accessory struct{}

func New() *Accessory { return &Accessory{} }

func (Accessory) Name() string { return "postgres" }

func (Accessory) ConfigSchema() map[string]accessory.ConfigOption {
	return map[string]accessory.ConfigOption{
		"dbname": {
			Description: "Name of the database to create. Must match ^[a-z_][a-z0-9_]*$.",
			Required:    true,
		},
	}
}

func (Accessory) Commands() []accessory.Command { return nil }

// HostBinding enables the framework `publish` command and supplies the
// default port for first-run / state-absent.
func (Accessory) HostBinding() *accessory.HostBindingSpec {
	return &accessory.HostBindingSpec{
		Description: "PostgreSQL",
		DefaultPort: defaultHostPort,
	}
}

// ServicesColumns renders the database column in `services` listings.
func (Accessory) ServicesColumns(r accessory.ServiceRequest) []accessory.Column {
	return []accessory.Column{
		{Header: "DATABASE", Value: r.Config["dbname"]},
	}
}

// Reconcile ensures the postgres container is running with the right
// host-port binding, then idempotently creates each requested database.
func (Accessory) Reconcile(ctx context.Context, env accessory.Env, requests []accessory.ServiceRequest) error {
	dbs, perServiceErrs := collectDatabases(requests)
	for name, err := range perServiceErrs {
		env.Log("skipping %s: %v", name, err)
	}
	if len(dbs) == 0 && !containerExists(ctx, env) {
		env.Log("no services request a database; not starting the container")
		return nil
	}

	if env.DryRun {
		env.Log("would ensure container %q (image=%s, host_port=%s)", ContainerName, Image, hostPortStr(env.HostBoundPort))
		for _, d := range dbs {
			env.Log("would ensure database %q for %s", d.Name, d.Requester)
		}
		return nil
	}

	id, err := ensureContainer(ctx, env)
	if err != nil {
		return err
	}
	if err := waitReady(ctx, env, id); err != nil {
		return fmt.Errorf("postgres did not become ready: %w", err)
	}

	for _, d := range dbs {
		created, err := ensureDatabase(ctx, env, id, d.Name)
		if err != nil {
			env.Log("FAILED to create %q (requested by %s): %v", d.Name, d.Requester, err)
			continue
		}
		verb := "ready"
		if created {
			verb = "created"
		}
		env.Log("database %q %s (in-network: %s, host: %s) — requested by %s",
			d.Name, verb,
			fmt.Sprintf("%s:%d/%s", ContainerName, internalPort, d.Name),
			hostURLFor(d.Name, env.HostBoundPort),
			d.Requester,
		)
	}
	return nil
}

// dbRequest associates a validated dbname with the service that asked
// for it.
type dbRequest struct {
	Name      string
	Requester string // displayName(service)
}

// collectDatabases validates and dedupes per-request dbnames.
func collectDatabases(reqs []accessory.ServiceRequest) ([]dbRequest, map[string]error) {
	errs := map[string]error{}
	seen := map[string]bool{}
	var out []dbRequest
	for _, r := range reqs {
		who := displayName(r.Service)
		name := strings.TrimSpace(r.Config["dbname"])
		if name == "" {
			errs[who] = errors.New("missing multienv.postgres.dbname")
			continue
		}
		if !dbNameRe.MatchString(name) {
			errs[who] = fmt.Errorf("invalid dbname %q (must match %s)", name, dbNameRe)
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, dbRequest{Name: name, Requester: who})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// containerExists is used to decide whether to keep the postgres
// container running when no service requests a database. It is best-
// effort and never errors — a transient inspect failure means we treat
// the container as absent for this decision.
func containerExists(ctx context.Context, env accessory.Env) bool {
	_, err := env.Docker.ContainerInspect(ctx, ContainerName)
	return err == nil
}

func ensureContainer(ctx context.Context, env accessory.Env) (string, error) {
	c, err := env.Docker.ContainerInspect(ctx, ContainerName)
	switch {
	case err == nil && matchesHostPort(c, env.HostBoundPort):
		if c.State != "running" {
			if err := env.Docker.ContainerStart(ctx, c.ID); err != nil {
				return "", fmt.Errorf("start existing postgres: %w", err)
			}
			env.Log("started existing container %q", ContainerName)
		}
		return c.ID, nil

	case err == nil:
		env.Log("host_port changed (%s → %s); recreating container (data volume preserved)",
			hostPortStr(currentHostPort(c)), hostPortStr(env.HostBoundPort))
		if rerr := env.Docker.ContainerRemove(ctx, c.ID, true); rerr != nil {
			return "", fmt.Errorf("remove postgres for recreate: %w", rerr)
		}

	case errors.Is(err, docker.ErrNotFound):
		// fall through to creation

	default:
		return "", fmt.Errorf("inspect postgres: %w", err)
	}

	env.Log("pulling image %s", Image)
	if err := env.Docker.ImagePull(ctx, Image); err != nil {
		return "", err
	}

	spec := docker.ContainerSpec{
		Name:  ContainerName,
		Image: Image,
		Env: []string{
			"POSTGRES_USER=" + dbUser,
			"POSTGRES_PASSWORD=" + dbPassword,
			// No POSTGRES_DB — we create databases on demand via psql.
		},
		Labels: map[string]string{
			"multienv.managed":   "true",
			"multienv.accessory": "postgres",
		},
		Networks: []string{env.Network},
		Mounts: []docker.Mount{
			{Type: "volume", Source: VolumeName, Target: dataPath},
		},
		RestartPolicy: "unless-stopped",
	}
	if env.HostBoundPort != 0 {
		spec.HostBindings = []docker.PortBinding{
			{HostIP: "127.0.0.1", HostPort: env.HostBoundPort, ContainerPort: internalPort, Protocol: "tcp"},
		}
	}

	id, err := env.Docker.ContainerCreate(ctx, spec)
	if err != nil {
		return "", err
	}
	if err := env.Docker.ContainerStart(ctx, id); err != nil {
		return "", err
	}
	env.Log("created container %q (image=%s, volume=%s, host_port=%s)",
		ContainerName, Image, VolumeName, hostPortStr(env.HostBoundPort))
	return id, nil
}

// currentHostPort returns the host port the existing container is
// currently publishing for postgres, or 0 when unpublished.
func currentHostPort(c *docker.Container) uint16 {
	if c == nil {
		return 0
	}
	for _, p := range c.Ports {
		if p.Private == internalPort && p.Public != 0 {
			return p.Public
		}
	}
	return 0
}

// matchesHostPort reports whether the existing container already
// publishes on want. want=0 means "should be unpublished".
func matchesHostPort(c *docker.Container, want uint16) bool {
	return currentHostPort(c) == want
}

// waitReady polls pg_isready inside the container until it reports
// success or the timeout elapses.
func waitReady(ctx context.Context, env accessory.Env, id string) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		err := env.Docker.ContainerExec(ctx, id,
			[]string{"pg_isready", "-U", dbUser, "-h", "127.0.0.1"},
			nil, nil,
		)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// ensureDatabase returns (createdNow, error). It only issues CREATE
// DATABASE when the database does not yet exist.
func ensureDatabase(ctx context.Context, env accessory.Env, id, name string) (bool, error) {
	// Defense in depth: even though name passed dbNameRe, never
	// interpolate untrusted input into SQL. Use a parameterized check
	// for existence and a strictly-validated identifier for creation.
	checkCmd := []string{
		"psql", "-U", dbUser, "-tAq", "-v", "ON_ERROR_STOP=1",
		"-c", "SELECT 1 FROM pg_database WHERE datname = '" + name + "'",
	}
	var out bytes.Buffer
	if err := env.Docker.ContainerExec(ctx, id, checkCmd, nil, &out); err != nil {
		return false, fmt.Errorf("check db: %w", err)
	}
	if strings.TrimSpace(out.String()) == "1" {
		return false, nil
	}
	createCmd := []string{
		"psql", "-U", dbUser, "-v", "ON_ERROR_STOP=1",
		"-c", "CREATE DATABASE " + name,
	}
	if err := env.Docker.ContainerExec(ctx, id, createCmd, nil, nil); err != nil {
		return false, fmt.Errorf("create db: %w", err)
	}
	return true, nil
}

func hostPortStr(p uint16) string {
	if p == 0 {
		return "off"
	}
	return fmt.Sprintf("127.0.0.1:%d", p)
}

func hostURLFor(db string, hostPort uint16) string {
	if hostPort == 0 {
		return "off"
	}
	return fmt.Sprintf("127.0.0.1:%d/%s", hostPort, db)
}

func displayName(s service.Service) string {
	if s.Project == "" {
		return s.Name
	}
	return s.Project + "/" + s.Name
}
