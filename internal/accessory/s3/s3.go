// Package s3 implements multienv's S3-compatible object-store accessory.
// Services declare multienv.s3.bucket=<name> on their container; this
// package owns a singleton MinIO container and idempotently creates the
// requested buckets inside it.
//
// Connection convention (no env injection — service author hardcodes):
//
//	in-network: http://minioadmin:minioadmin@multienv-s3:9000
//	from host:  http://minioadmin:minioadmin@127.0.0.1:<host_port>
package s3

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

const (
	ContainerName     = "multienv-s3"
	Image             = "minio/minio:latest"
	VolumeName        = "multienv-s3-data"
	dataPath          = "/data"
	accessKey         = "minio"
	secretKey         = "minio123"
	internalPort      = 9000
	consolePort       = 9001
	defaultHostPort   = 9000
	readyTimeout      = 30 * time.Second
	readyPollInterval = 1 * time.Second
)

// bucketNameRe matches the bucket-name format multienv accepts. Strict
// on purpose: passes straight into `mc mb`, no quoting tricks. Subset of
// the full S3 rules (3-63 chars, lowercase alphanumeric and hyphens,
// must start and end with alphanumeric).
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// Accessory is the S3 accessory implementation, backed by MinIO.
type Accessory struct{}

func New() *Accessory { return &Accessory{} }

func (Accessory) Name() string { return "s3" }

func (Accessory) ConfigSchema() map[string]accessory.ConfigOption {
	return map[string]accessory.ConfigOption{
		"bucket": {
			Description: "Name of the bucket to create. Must match ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$.",
			Required:    true,
		},
		"domain": {
			Description: "Optional public-facing hostname. When set, the proxy accessory routes " +
				"this domain to the shared S3 endpoint.",
			Required: false,
		},
		"public": {
			Description: "When true, the bucket allows anonymous read (anyone can download). Defaults to private.",
			Required:    false,
			Default:     "false",
		},
	}
}

func (Accessory) Commands() []accessory.Command { return nil }

// HostBinding enables the framework `publish` command and supplies the
// default port for first-run / state-absent. Only the S3 API port is
// host-bound; the MinIO console stays in-network only.
func (Accessory) HostBinding() *accessory.HostBindingSpec {
	return &accessory.HostBindingSpec{
		Description: "S3 (MinIO)",
		DefaultPort: defaultHostPort,
	}
}

// ServicesColumns renders the bucket column in `services` listings.
func (Accessory) ServicesColumns(r accessory.ServiceRequest) []accessory.Column {
	public, _ := parsePublicFlag(r.Config["public"])
	return []accessory.Column{
		{Header: "BUCKET", Value: r.Config["bucket"]},
		{Header: "DOMAIN", Value: r.Config["domain"]},
		{Header: "PUBLIC", Value: strconv.FormatBool(public)},
	}
}

// Reconcile ensures the MinIO container is running with the right
// host-port binding, then idempotently creates each requested bucket.
func (Accessory) Reconcile(ctx context.Context, env accessory.Env, requests []accessory.ServiceRequest) error {
	buckets, bucketErrs := collectBuckets(requests)
	for name, err := range bucketErrs {
		env.Log("skipping bucket for %s: %v", name, err)
	}
	domains, domainErrs := collectDomains(requests)
	for name, err := range domainErrs {
		env.Log("skipping domain for %s: %v", name, err)
	}
	if len(buckets) == 0 && len(domains) == 0 && !containerExists(ctx, env) {
		env.Log("no services request a bucket or domain; not starting the container")
		return nil
	}

	if env.DryRun {
		env.Log("would ensure container %q (image=%s, host_port=%s, proxy_domains=%s)",
			ContainerName, Image, hostPortStr(env.HostBoundPort), domainLabel(domains))
		for _, b := range buckets {
			env.Log("would ensure bucket %q (policy=%s) for %s", b.Name, policyLabel(b.Public), b.Requester)
		}
		for _, d := range domains {
			env.Log("would route %s → %s:%d (via proxy)", d, ContainerName, internalPort)
		}
		return nil
	}

	id, err := ensureContainer(ctx, env, domains)
	if err != nil {
		return err
	}
	for _, d := range domains {
		env.Log("routing %s → %s:%d (via proxy)", d, ContainerName, internalPort)
	}
	if err := waitReady(ctx, env, id); err != nil {
		return fmt.Errorf("minio did not become ready: %w", err)
	}

	for _, b := range buckets {
		created, err := ensureBucket(ctx, env, id, b.Name)
		if err != nil {
			env.Log("FAILED to create %q (requested by %s): %v", b.Name, b.Requester, err)
			continue
		}
		if err := setBucketPolicy(ctx, env, id, b.Name, b.Public); err != nil {
			env.Log("FAILED to set policy on %q (requested by %s): %v", b.Name, b.Requester, err)
			continue
		}
		verb := "ready"
		if created {
			verb = "created"
		}
		env.Log("bucket %q %s policy=%s (in-network: %s, host: %s) — requested by %s",
			b.Name, verb, policyLabel(b.Public),
			fmt.Sprintf("%s:%d/%s", ContainerName, internalPort, b.Name),
			hostURLFor(b.Name, env.HostBoundPort),
			b.Requester,
		)
	}
	return nil
}

// bucketRequest associates a validated bucket name with the service
// that asked for it and the desired anonymous-read state.
type bucketRequest struct {
	Name      string
	Requester string // displayName(service)
	Public    bool
}

// parsePublicFlag parses the multienv.s3.public label value. Empty
// strings default to false; anything else must be a strconv.ParseBool-
// compatible value.
func parsePublicFlag(raw string) (bool, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("invalid multienv.s3.public %q (want true/false)", raw)
	}
	return b, nil
}

// domainRe matches the domain format multienv accepts. Pragmatic
// hostname check — commas are forbidden because we comma-join domains
// into a single label value.
var domainRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*$`)

// collectDomains validates and dedupes per-request domains. Returns the
// sorted unique list and a per-service map of validation errors.
func collectDomains(reqs []accessory.ServiceRequest) ([]string, map[string]error) {
	errs := map[string]error{}
	seen := map[string]bool{}
	var out []string
	for _, r := range reqs {
		who := displayName(r.Service)
		raw := strings.TrimSpace(r.Config["domain"])
		if raw == "" {
			continue
		}
		if !domainRe.MatchString(raw) {
			errs[who] = fmt.Errorf("invalid domain %q", raw)
			continue
		}
		if seen[raw] {
			continue
		}
		seen[raw] = true
		out = append(out, raw)
	}
	sort.Strings(out)
	return out, errs
}

// domainLabel renders the multienv.proxy.domain label value (empty when
// no domains are requested).
func domainLabel(domains []string) string {
	return strings.Join(domains, ",")
}

// collectBuckets validates and dedupes per-request bucket names. On
// duplicate names with conflicting public flags the first requester
// wins; the conflict is recorded in errs so callers can surface it.
func collectBuckets(reqs []accessory.ServiceRequest) ([]bucketRequest, map[string]error) {
	errs := map[string]error{}
	idx := map[string]int{}
	var out []bucketRequest
	for _, r := range reqs {
		who := displayName(r.Service)
		name := strings.TrimSpace(r.Config["bucket"])
		if name == "" {
			errs[who] = errors.New("missing multienv.s3.bucket")
			continue
		}
		if !bucketNameRe.MatchString(name) {
			errs[who] = fmt.Errorf("invalid bucket %q (must match %s)", name, bucketNameRe)
			continue
		}
		public, err := parsePublicFlag(r.Config["public"])
		if err != nil {
			errs[who] = err
			continue
		}
		if existing, dup := idx[name]; dup {
			if out[existing].Public != public {
				errs[who] = fmt.Errorf("conflicting public flag for bucket %q (keeping %v from %s)",
					name, out[existing].Public, out[existing].Requester)
			}
			continue
		}
		idx[name] = len(out)
		out = append(out, bucketRequest{Name: name, Requester: who, Public: public})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, errs
}

// containerExists is used to decide whether to keep the MinIO container
// running when no service requests a bucket. Best-effort; transient
// inspect failures are treated as "absent" for this decision only.
func containerExists(ctx context.Context, env accessory.Env) bool {
	_, err := env.Docker.ContainerInspect(ctx, ContainerName)
	return err == nil
}

func ensureContainer(ctx context.Context, env accessory.Env, domains []string) (string, error) {
	c, err := env.Docker.ContainerInspect(ctx, ContainerName)
	switch {
	case err == nil && matchesDesired(c, env.HostBoundPort, domains):
		if c.State != "running" {
			if err := env.Docker.ContainerStart(ctx, c.ID); err != nil {
				return "", fmt.Errorf("start existing minio: %w", err)
			}
			env.Log("started existing container %q", ContainerName)
		}
		return c.ID, nil

	case err == nil:
		logRecreateReason(env, c, domains)
		if rerr := env.Docker.ContainerRemove(ctx, c.ID, true); rerr != nil {
			return "", fmt.Errorf("remove minio for recreate: %w", rerr)
		}

	case errors.Is(err, docker.ErrNotFound):
		// fall through to creation

	default:
		return "", fmt.Errorf("inspect minio: %w", err)
	}

	env.Log("pulling image %s", Image)
	if err := env.Docker.ImagePull(ctx, Image); err != nil {
		return "", err
	}

	labels := map[string]string{
		"multienv.managed":   "true",
		"multienv.accessory": "s3",
	}
	if len(domains) > 0 {
		// Stamp the multienv-s3 container itself with proxy labels. The
		// proxy accessory sees it as a service and routes every listed
		// domain to multienv-s3:9000.
		labels["multienv.proxy.domain"] = domainLabel(domains)
		labels["multienv.proxy.port"] = fmt.Sprintf("%d", internalPort)
	}
	spec := docker.ContainerSpec{
		Name:  ContainerName,
		Image: Image,
		Cmd:   []string{"server", dataPath, "--console-address", fmt.Sprintf(":%d", consolePort)},
		Env: []string{
			"MINIO_ROOT_USER=" + accessKey,
			"MINIO_ROOT_PASSWORD=" + secretKey,
			// Pre-baked mc alias: lets ContainerExec call `mc` directly
			// without a shell wrapper or per-call alias setup.
			fmt.Sprintf("MC_HOST_local=http://%s:%s@127.0.0.1:%d", accessKey, secretKey, internalPort),
		},
		Labels:   labels,
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
// currently publishing for the S3 API, or 0 when unpublished.
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

// currentProxyDomain returns the multienv.proxy.domain label value
// currently stamped on the container (empty string if absent).
func currentProxyDomain(c *docker.Container) string {
	if c == nil {
		return ""
	}
	return c.Labels["multienv.proxy.domain"]
}

// matchesDesired reports whether the existing container already matches
// both the desired host port and the desired proxy-domain label.
func matchesDesired(c *docker.Container, hostPort uint16, domains []string) bool {
	return matchesHostPort(c, hostPort) && currentProxyDomain(c) == domainLabel(domains)
}

// logRecreateReason emits one log line per piece of drift between the
// live container and the desired state, then announces the recreate.
func logRecreateReason(env accessory.Env, c *docker.Container, domains []string) {
	if !matchesHostPort(c, env.HostBoundPort) {
		env.Log("host_port changed (%s → %s)",
			hostPortStr(currentHostPort(c)), hostPortStr(env.HostBoundPort))
	}
	if currentProxyDomain(c) != domainLabel(domains) {
		env.Log("proxy domains changed (%q → %q)",
			currentProxyDomain(c), domainLabel(domains))
	}
	env.Log("recreating container (data volume preserved)")
}

// waitReady polls `mc ready` inside the container until it reports
// success or the timeout elapses.
func waitReady(ctx context.Context, env accessory.Env, id string) error {
	deadline := time.Now().Add(readyTimeout)
	for {
		err := env.Docker.ContainerExec(ctx, id,
			[]string{"mc", "ready", "local"},
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

// setBucketPolicy applies the anonymous-read policy. mc anonymous set
// is idempotent, so we apply unconditionally rather than reading first.
func setBucketPolicy(ctx context.Context, env accessory.Env, id, name string, public bool) error {
	perm := "none"
	if public {
		perm = "download"
	}
	cmd := []string{"mc", "anonymous", "set", perm, "local/" + name}
	if err := env.Docker.ContainerExec(ctx, id, cmd, nil, nil); err != nil {
		return fmt.Errorf("mc anonymous set %s: %w", perm, err)
	}
	return nil
}

// policyLabel is the short human-readable name for the bucket's
// anonymous-read state, used in log lines.
func policyLabel(public bool) string {
	if public {
		return "public"
	}
	return "private"
}

// ensureBucket returns (createdNow, error). It only issues `mc mb` when
// the bucket does not yet exist.
func ensureBucket(ctx context.Context, env accessory.Env, id, name string) (bool, error) {
	// Defense in depth: name has already passed bucketNameRe, but never
	// trust an unvalidated identifier as a path segment.
	checkCmd := []string{"mc", "stat", "local/" + name}
	if err := env.Docker.ContainerExec(ctx, id, checkCmd, nil, nil); err == nil {
		return false, nil
	}
	createCmd := []string{"mc", "mb", "local/" + name}
	if err := env.Docker.ContainerExec(ctx, id, createCmd, nil, nil); err != nil {
		return false, fmt.Errorf("create bucket: %w", err)
	}
	return true, nil
}

func hostPortStr(p uint16) string {
	if p == 0 {
		return "off"
	}
	return fmt.Sprintf("127.0.0.1:%d", p)
}

func hostURLFor(bucket string, hostPort uint16) string {
	if hostPort == 0 {
		return "off"
	}
	return fmt.Sprintf("127.0.0.1:%d/%s", hostPort, bucket)
}

func displayName(s service.Service) string {
	if s.Project == "" {
		return s.Name
	}
	return s.Project + "/" + s.Name
}
