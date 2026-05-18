package postgres

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

func req(name string, dbname string) accessory.ServiceRequest {
	return accessory.ServiceRequest{
		Service: service.Service{Name: name, Project: "myapp", ContainerName: "myapp-" + name + "-1"},
		Config:  map[string]string{"dbname": dbname},
	}
}

func TestCollectDatabases_HappyPath(t *testing.T) {
	t.Parallel()
	got, errs := collectDatabases([]accessory.ServiceRequest{
		req("worker", "jobs"),
		req("api", "myapp"),
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "jobs" || got[1].Name != "myapp" {
		t.Errorf("not sorted by dbname: %+v", got)
	}
	if got[1].Requester != "myapp/api" {
		t.Errorf("requester = %q, want myapp/api", got[1].Requester)
	}
}

func TestCollectDatabases_DedupesSameDbname(t *testing.T) {
	t.Parallel()
	got, errs := collectDatabases([]accessory.ServiceRequest{
		req("api", "shared"),
		req("worker", "shared"),
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (deduped)", len(got))
	}
}

func TestCollectDatabases_RejectsInvalidNames(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":         "",
		"uppercase":     "MyDb",
		"hyphen":        "my-db",
		"leading digit": "1db",
		"sql injection": "x; drop database postgres;",
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			_, errs := collectDatabases([]accessory.ServiceRequest{req("api", name)})
			if _, bad := errs["myapp/api"]; !bad {
				t.Errorf("expected error for dbname %q, got none", name)
			}
		})
	}
}

func TestCollectDatabases_AcceptsValidNames(t *testing.T) {
	t.Parallel()
	good := []string{"myapp", "my_app", "_internal", "a", "a1", "long_name_with_digits_1234"}
	for _, n := range good {
		dbs, errs := collectDatabases([]accessory.ServiceRequest{req("api", n)})
		if len(errs) != 0 || len(dbs) != 1 {
			t.Errorf("rejected valid name %q: errs=%v dbs=%v", n, errs, dbs)
		}
	}
}

func TestConfigSchema_DbnameRequired(t *testing.T) {
	t.Parallel()
	s := Accessory{}.ConfigSchema()
	if !s["dbname"].Required {
		t.Error("dbname must be marked required")
	}
}

func TestHostBinding_DefaultsTo5432(t *testing.T) {
	t.Parallel()
	hb := Accessory{}.HostBinding()
	if hb == nil {
		t.Fatal("HostBinding must not be nil for postgres")
	}
	if hb.DefaultPort != 5432 {
		t.Errorf("DefaultPort = %d, want 5432", hb.DefaultPort)
	}
}

func TestServicesColumns_ExposesDatabase(t *testing.T) {
	t.Parallel()
	cols := Accessory{}.ServicesColumns(accessory.ServiceRequest{Config: map[string]string{"dbname": "x"}})
	if len(cols) != 1 || cols[0].Header != "DATABASE" || cols[0].Value != "x" {
		t.Errorf("ServicesColumns = %+v, want DATABASE=x", cols)
	}
}

// fakeDocker is a minimal docker.API stub for postgres lifecycle tests.
// It records the call sequence so tests can assert ordering.
type fakeDocker struct {
	current *docker.Container // returned by ContainerInspect (nil = NotFound)

	calls       []string // ordered: "inspect" / "remove:<id>" / "pull" / "create:<name>" / "start:<id>" / "exec"
	psqlReplies map[string]string
}

func (f *fakeDocker) ListContainers(context.Context) ([]docker.Container, error) {
	return nil, nil
}
func (f *fakeDocker) NetworkInspect(context.Context, string) (*docker.Network, error) {
	return nil, nil
}
func (f *fakeDocker) NetworkCreate(context.Context, docker.NetworkSpec) (string, error) {
	return "", nil
}
func (f *fakeDocker) NetworkConnect(context.Context, string, string) error { return nil }

func (f *fakeDocker) ContainerInspect(_ context.Context, name string) (*docker.Container, error) {
	f.calls = append(f.calls, "inspect:"+name)
	if f.current == nil {
		return nil, docker.ErrNotFound
	}
	return f.current, nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, spec docker.ContainerSpec) (string, error) {
	f.calls = append(f.calls, "create:"+spec.Name)
	id := "new-" + spec.Name
	f.current = &docker.Container{
		ID:    id,
		Names: []string{"/" + spec.Name},
		State: "created",
	}
	return id, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string) error {
	f.calls = append(f.calls, "start:"+id)
	if f.current != nil {
		f.current.State = "running"
	}
	return nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ bool) error {
	f.calls = append(f.calls, "remove:"+id)
	f.current = nil
	return nil
}

func (f *fakeDocker) ContainerExec(_ context.Context, _ string, cmd []string, _ io.Reader, stdout io.Writer) error {
	if len(cmd) > 0 && cmd[0] == "pg_isready" {
		f.calls = append(f.calls, "ready")
		return nil
	}
	joined := strings.Join(cmd, " ")
	f.calls = append(f.calls, "exec")
	if reply, ok := f.psqlReplies[joined]; ok && stdout != nil {
		_, _ = stdout.Write([]byte(reply))
	}
	return nil
}

func (f *fakeDocker) ContainerCopyFrom(context.Context, string, string, io.Writer) error {
	return errors.New("not used")
}

func (f *fakeDocker) ImagePull(_ context.Context, _ string) error {
	f.calls = append(f.calls, "pull")
	return nil
}

func TestEnsureContainer_RecreatesOnHostPortMismatch(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		current: &docker.Container{
			ID:    "old-id",
			Names: []string{"/" + ContainerName},
			State: "running",
			Ports: []docker.ContainerPort{
				{Private: internalPort, Public: 5432, Protocol: "tcp"},
			},
		},
	}
	var logBuf bytes.Buffer
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 5433, // mismatch
		Log: func(format string, args ...any) {
			logBuf.WriteString(format + "\n")
		},
	}

	id, err := ensureContainer(context.Background(), env)
	if err != nil {
		t.Fatalf("ensureContainer: %v", err)
	}
	if id != "new-"+ContainerName {
		t.Errorf("returned id = %q, want new-%s", id, ContainerName)
	}

	wantSeq := []string{
		"inspect:" + ContainerName,
		"remove:old-id",
		"pull",
		"create:" + ContainerName,
		"start:new-" + ContainerName,
	}
	if !equalSeq(fd.calls, wantSeq) {
		t.Errorf("call sequence = %v, want %v", fd.calls, wantSeq)
	}
	if !strings.Contains(logBuf.String(), "host_port changed") {
		t.Errorf("expected host_port-change log line, got: %s", logBuf.String())
	}
}

func TestEnsureContainer_StartsExistingOnMatch(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		current: &docker.Container{
			ID:    "id1",
			Names: []string{"/" + ContainerName},
			State: "exited",
			Ports: []docker.ContainerPort{
				{Private: internalPort, Public: 5432, Protocol: "tcp"},
			},
		},
	}
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 5432,
		Log:           func(string, ...any) {},
	}
	id, err := ensureContainer(context.Background(), env)
	if err != nil {
		t.Fatalf("ensureContainer: %v", err)
	}
	if id != "id1" {
		t.Errorf("id = %q, want id1", id)
	}
	wantSeq := []string{"inspect:" + ContainerName, "start:id1"}
	if !equalSeq(fd.calls, wantSeq) {
		t.Errorf("call sequence = %v, want %v", fd.calls, wantSeq)
	}
}

func equalSeq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestMatchesHostPort(t *testing.T) {
	t.Parallel()
	c := &docker.Container{Ports: []docker.ContainerPort{
		{Private: 5432, Public: 5432, Protocol: "tcp"},
	}}
	if !matchesHostPort(c, 5432) {
		t.Errorf("expected match on 5432")
	}
	if matchesHostPort(c, 5433) {
		t.Errorf("did not expect match on 5433")
	}
	if matchesHostPort(c, 0) {
		t.Errorf("did not expect match for 'off' when port is published")
	}
	empty := &docker.Container{}
	if !matchesHostPort(empty, 0) {
		t.Errorf("expected match for 'off' on unpublished container")
	}
}
