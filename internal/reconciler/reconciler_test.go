package reconciler

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lowski/multienv/internal/docker"
)

// fakeDocker is a controllable in-memory stand-in for docker.Client.
type fakeDocker struct {
	containers []docker.Container

	network *docker.Network // nil => not found

	createCalls  []docker.NetworkSpec
	connectCalls []connectCall
	createErr    error
	connectErr   map[string]error // keyed by container ID
}

type connectCall struct {
	networkID, containerID string
}

func (f *fakeDocker) ListContainers(_ context.Context) ([]docker.Container, error) {
	return f.containers, nil
}

func (f *fakeDocker) NetworkInspect(_ context.Context, name string) (*docker.Network, error) {
	if f.network == nil || f.network.Name != name {
		return nil, docker.ErrNotFound
	}
	return f.network, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, spec docker.NetworkSpec) (string, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.createCalls = append(f.createCalls, spec)
	id := "net-" + spec.Name
	f.network = &docker.Network{
		ID:                   id,
		Name:                 spec.Name,
		Labels:               spec.Labels,
		AttachedContainerIDs: map[string]struct{}{},
	}
	return id, nil
}

func (f *fakeDocker) NetworkConnect(_ context.Context, networkID, containerID string) error {
	if err := f.connectErr[containerID]; err != nil {
		return err
	}
	f.connectCalls = append(f.connectCalls, connectCall{networkID, containerID})
	return nil
}

func multienvContainer(id, project, name string) docker.Container {
	return docker.Container{
		ID:    id,
		Names: []string{"/" + name},
		Labels: map[string]string{
			"com.docker.compose.project": project,
			"com.docker.compose.service": name,
			"multienv.proxy.domain":      name + ".example.com",
		},
	}
}

func TestReconcile_CreatesNetworkAndAttachesAllServices(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		containers: []docker.Container{
			multienvContainer("c-api", "myapp", "api"),
			multienvContainer("c-worker", "myapp", "worker"),
			{ID: "c-other", Names: []string{"/other"}, Labels: map[string]string{"unrelated": "1"}},
		},
	}
	var out bytes.Buffer
	r := &Reconciler{Docker: fd, Out: &out}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(fd.createCalls) != 1 {
		t.Fatalf("createCalls = %d, want 1", len(fd.createCalls))
	}
	if got := fd.createCalls[0]; got.Name != NetworkName || got.Driver != "bridge" || !got.Attachable {
		t.Errorf("create spec = %+v, want name=%s driver=bridge attachable=true", got, NetworkName)
	}
	if got := fd.createCalls[0].Labels[ManagedLabel]; got != "true" {
		t.Errorf("managed label = %q, want \"true\"", got)
	}

	connected := map[string]bool{}
	for _, c := range fd.connectCalls {
		connected[c.containerID] = true
	}
	if !connected["c-api"] || !connected["c-worker"] {
		t.Errorf("expected api and worker to be connected, got %v", connected)
	}
	if connected["c-other"] {
		t.Error("non-multienv container should not be connected")
	}
	if !strings.Contains(out.String(), "created network") {
		t.Errorf("log missing network-create line: %s", out.String())
	}
}

func TestReconcile_SkipsAlreadyAttached(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		network: &docker.Network{
			ID:   "net-multienv",
			Name: NetworkName,
			AttachedContainerIDs: map[string]struct{}{
				"c-api": {},
			},
		},
		containers: []docker.Container{
			multienvContainer("c-api", "myapp", "api"),
			multienvContainer("c-worker", "myapp", "worker"),
		},
	}
	r := &Reconciler{Docker: fd, Out: &bytes.Buffer{}}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(fd.createCalls) != 0 {
		t.Errorf("createCalls = %d, want 0 (network already exists)", len(fd.createCalls))
	}
	if len(fd.connectCalls) != 1 || fd.connectCalls[0].containerID != "c-worker" {
		t.Errorf("connectCalls = %+v, want one call for c-worker", fd.connectCalls)
	}
}

func TestReconcile_DryRunDoesNothing(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		containers: []docker.Container{
			multienvContainer("c-api", "myapp", "api"),
		},
	}
	var out bytes.Buffer
	r := &Reconciler{Docker: fd, Out: &out, DryRun: true}

	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(fd.createCalls) != 0 {
		t.Errorf("createCalls = %d, want 0 in dry-run", len(fd.createCalls))
	}
	if len(fd.connectCalls) != 0 {
		t.Errorf("connectCalls = %d, want 0 in dry-run", len(fd.connectCalls))
	}
	log := out.String()
	if !strings.Contains(log, "[housekeeping] would create network") {
		t.Errorf("dry-run log missing housekeeping line: %s", log)
	}
	if !strings.Contains(log, "[service myapp/api] would attach") {
		t.Errorf("dry-run log missing service line: %s", log)
	}
}

func TestReconcile_AggregatesPerServiceErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	fd := &fakeDocker{
		network: &docker.Network{
			ID:                   "net-multienv",
			Name:                 NetworkName,
			AttachedContainerIDs: map[string]struct{}{},
		},
		containers: []docker.Container{
			multienvContainer("c-api", "myapp", "api"),
			multienvContainer("c-worker", "myapp", "worker"),
		},
		connectErr: map[string]error{"c-api": boom},
	}
	r := &Reconciler{Docker: fd, Out: &bytes.Buffer{}}

	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("expected error from failing service")
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want it to wrap boom", err)
	}

	// The healthy service should still have been attached.
	if len(fd.connectCalls) != 1 || fd.connectCalls[0].containerID != "c-worker" {
		t.Errorf("connectCalls = %+v, want one call for c-worker", fd.connectCalls)
	}
}
