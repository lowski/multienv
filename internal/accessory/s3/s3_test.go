package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

func req(name string, bucket string) accessory.ServiceRequest {
	return accessory.ServiceRequest{
		Service: service.Service{Name: name, Project: "myapp", ContainerName: "myapp-" + name + "-1"},
		Config:  map[string]string{"bucket": bucket},
	}
}

func TestCollectBuckets_HappyPath(t *testing.T) {
	t.Parallel()
	got, errs := collectBuckets([]accessory.ServiceRequest{
		req("worker", "uploads"),
		req("api", "assets"),
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Name != "assets" || got[1].Name != "uploads" {
		t.Errorf("not sorted by bucket: %+v", got)
	}
	if got[0].Requester != "myapp/api" {
		t.Errorf("requester = %q, want myapp/api", got[0].Requester)
	}
}

func TestCollectBuckets_DedupesSameBucket(t *testing.T) {
	t.Parallel()
	got, errs := collectBuckets([]accessory.ServiceRequest{
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

func TestCollectBuckets_RejectsInvalidNames(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":           "",
		"uppercase":       "MyBucket",
		"underscore":      "my_bucket",
		"too short":       "ab",
		"leading hyphen":  "-bucket",
		"trailing hyphen": "bucket-",
		"dot":             "my.bucket",
		"sql injection":   "x; drop bucket;",
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			_, errs := collectBuckets([]accessory.ServiceRequest{req("api", name)})
			if _, bad := errs["myapp/api"]; !bad {
				t.Errorf("expected error for bucket %q, got none", name)
			}
		})
	}
}

func TestCollectBuckets_AcceptsValidNames(t *testing.T) {
	t.Parallel()
	good := []string{"abc", "my-bucket", "uploads", "a1b", "long-name-with-digits-1234"}
	for _, n := range good {
		buckets, errs := collectBuckets([]accessory.ServiceRequest{req("api", n)})
		if len(errs) != 0 || len(buckets) != 1 {
			t.Errorf("rejected valid name %q: errs=%v buckets=%v", n, errs, buckets)
		}
	}
}

func TestConfigSchema_BucketRequired(t *testing.T) {
	t.Parallel()
	s := Accessory{}.ConfigSchema()
	if !s["bucket"].Required {
		t.Error("bucket must be marked required")
	}
}

func TestHostBinding_DefaultsTo9000(t *testing.T) {
	t.Parallel()
	hb := Accessory{}.HostBinding()
	if hb == nil {
		t.Fatal("HostBinding must not be nil for s3")
	}
	if hb.DefaultPort != 9000 {
		t.Errorf("DefaultPort = %d, want 9000", hb.DefaultPort)
	}
}

func TestServicesColumns_ExposesBucketDomainPublic(t *testing.T) {
	t.Parallel()
	cols := Accessory{}.ServicesColumns(accessory.ServiceRequest{
		Config: map[string]string{"bucket": "x", "domain": "s3.example.com", "public": "true"},
	})
	if len(cols) != 3 {
		t.Fatalf("len = %d, want 3", len(cols))
	}
	want := []accessory.Column{
		{Header: "BUCKET", Value: "x"},
		{Header: "DOMAIN", Value: "s3.example.com"},
		{Header: "PUBLIC", Value: "true"},
	}
	for i, w := range want {
		if cols[i] != w {
			t.Errorf("col[%d] = %+v, want %+v", i, cols[i], w)
		}
	}
}

func TestParsePublicFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in      string
		want    bool
		wantErr bool
	}{
		"empty":   {"", false, false},
		"true":    {"true", true, false},
		"True":    {"True", true, false},
		"1":       {"1", true, false},
		"false":   {"false", false, false},
		"0":       {"0", false, false},
		"invalid": {"yeah", false, true},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			got, err := parsePublicFlag(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("parsePublicFlag(%q) = (%v, nil), want error", c.in, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("parsePublicFlag(%q) = (%v, %v), want (%v, nil)", c.in, got, err, c.want)
			}
		})
	}
}

func TestCollectBuckets_CarriesPublicFlag(t *testing.T) {
	t.Parallel()
	got, errs := collectBuckets([]accessory.ServiceRequest{
		{
			Service: service.Service{Name: "api", Project: "myapp"},
			Config:  map[string]string{"bucket": "uploads", "public": "true"},
		},
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 1 || !got[0].Public {
		t.Errorf("got %+v, want Public=true", got)
	}
}

func TestCollectBuckets_ConflictingPublicFlagReportsErrorAndFirstWins(t *testing.T) {
	t.Parallel()
	got, errs := collectBuckets([]accessory.ServiceRequest{
		{
			Service: service.Service{Name: "api", Project: "myapp"},
			Config:  map[string]string{"bucket": "shared", "public": "true"},
		},
		{
			Service: service.Service{Name: "worker", Project: "myapp"},
			Config:  map[string]string{"bucket": "shared", "public": "false"},
		},
	})
	if len(got) != 1 || !got[0].Public {
		t.Errorf("first requester's public=true should win, got %+v", got)
	}
	if _, ok := errs["myapp/worker"]; !ok {
		t.Errorf("expected conflict error for worker, got %v", errs)
	}
}

func TestCollectBuckets_InvalidPublicValueRejected(t *testing.T) {
	t.Parallel()
	_, errs := collectBuckets([]accessory.ServiceRequest{
		{
			Service: service.Service{Name: "api", Project: "myapp"},
			Config:  map[string]string{"bucket": "uploads", "public": "yeah"},
		},
	})
	if _, bad := errs["myapp/api"]; !bad {
		t.Errorf("expected error for invalid public value, got %v", errs)
	}
}

func TestSetBucketPolicy_MapsBoolToPolicyRequest(t *testing.T) {
	t.Parallel()
	cases := []struct {
		public bool
		want   string
	}{
		{true, "PUT /uploads?policy="},
		{false, "DELETE /uploads?policy="},
	}
	for _, c := range cases {
		fd := &fakeDocker{}
		env := accessory.Env{Docker: fd, Log: func(string, ...any) {}}
		if err := setBucketPolicy(context.Background(), env, "id", "uploads", c.public); err != nil {
			t.Fatalf("setBucketPolicy(%v): %v", c.public, err)
		}
		if len(fd.calls) != 1 || fd.calls[0] != c.want {
			t.Errorf("public=%v: calls = %v, want [%s]", c.public, fd.calls, c.want)
		}
	}
}

func TestSetBucketPolicy_PublicAllowsAnonymousGetObjectOnly(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	env := accessory.Env{Docker: fd, Log: func(string, ...any) {}}
	if err := setBucketPolicy(context.Background(), env, "id", "uploads", true); err != nil {
		t.Fatalf("setBucketPolicy: %v", err)
	}
	var policy struct {
		Statement []struct {
			Effect    string
			Principal map[string][]string
			Action    []string
			Resource  []string
		}
	}
	if err := json.Unmarshal([]byte(fd.lastBody), &policy); err != nil {
		t.Fatalf("policy body is not JSON: %v (%q)", err, fd.lastBody)
	}
	granted := map[string]string{}
	for _, st := range policy.Statement {
		if st.Effect != "Allow" || len(st.Principal["AWS"]) != 1 || st.Principal["AWS"][0] != "*" {
			t.Errorf("unexpected statement %+v", st)
		}
		for _, a := range st.Action {
			granted[a] = strings.Join(st.Resource, ",")
		}
	}
	if granted["s3:GetObject"] != "arn:aws:s3:::uploads/*" {
		t.Errorf("s3:GetObject resource = %q, want arn:aws:s3:::uploads/*", granted["s3:GetObject"])
	}
	if _, listable := granted["s3:ListBucket"]; listable {
		t.Error("public bucket must not be anonymously listable")
	}
}

func reqDomain(name, bucket, domain string) accessory.ServiceRequest {
	return accessory.ServiceRequest{
		Service: service.Service{Name: name, Project: "myapp", ContainerName: "myapp-" + name + "-1"},
		Config:  map[string]string{"bucket": bucket, "domain": domain},
	}
}

func TestCollectDomains_DedupesAndSorts(t *testing.T) {
	t.Parallel()
	got, errs := collectDomains([]accessory.ServiceRequest{
		reqDomain("api", "u", "z.example.com"),
		reqDomain("worker", "u", "a.example.com"),
		reqDomain("cron", "u", "a.example.com"), // duplicate
	})
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(got) != 2 || got[0] != "a.example.com" || got[1] != "z.example.com" {
		t.Errorf("got %v, want [a.example.com z.example.com]", got)
	}
}

func TestCollectDomains_OmitsEmpty(t *testing.T) {
	t.Parallel()
	got, errs := collectDomains([]accessory.ServiceRequest{req("api", "u")}) // no domain key
	if len(errs) != 0 || len(got) != 0 {
		t.Errorf("got %v errs=%v, want no domains and no errors", got, errs)
	}
}

func TestCollectDomains_RejectsInvalid(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"contains comma": "a.example.com,b.example.com",
		"contains space": "bad domain",
		"trailing dot":   "bad..example.com",
		"leading hyphen": "-bad.example.com",
	}
	for label, name := range cases {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			_, errs := collectDomains([]accessory.ServiceRequest{reqDomain("api", "u", name)})
			if _, bad := errs["myapp/api"]; !bad {
				t.Errorf("expected error for domain %q, got none", name)
			}
		})
	}
}

// fakeDocker is a minimal docker.API stub for s3 lifecycle tests. It
// records the call sequence so tests can assert ordering.
type fakeDocker struct {
	current *docker.Container // returned by ContainerInspect (nil = NotFound)

	calls    []string        // ordered: "inspect:<name>" / "remove:<id>" / "pull" / "create:<name>" / "start:<id>" / "<METHOD> <path>"
	buckets  map[string]bool // bucket paths ("/uploads") that exist
	lastBody string          // --data-binary of the most recent S3 request
	lastSpec *docker.ContainerSpec
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
	f.lastSpec = &spec
	id := "new-" + spec.Name
	f.current = &docker.Container{
		ID:     id,
		Names:  []string{"/" + spec.Name},
		Image:  spec.Image,
		State:  "created",
		Labels: spec.Labels,
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

// ContainerExec interprets the curl invocations the accessory issues,
// recording each as "<METHOD> <path>" relative to the local S3 API.
func (f *fakeDocker) ContainerExec(_ context.Context, _ string, cmd []string, _ io.Reader, _ io.Writer) error {
	if len(cmd) == 0 || cmd[0] != "curl" {
		f.calls = append(f.calls, "exec")
		return nil
	}
	method := "GET"
	for i, arg := range cmd {
		switch arg {
		case "-I":
			method = "HEAD"
		case "-X":
			method = cmd[i+1]
		case "--data-binary":
			f.lastBody = cmd[i+1]
		}
	}
	path := strings.TrimPrefix(cmd[len(cmd)-1], localURL(""))
	f.calls = append(f.calls, method+" "+path)

	switch method {
	case "HEAD":
		if !f.buckets[path] {
			return errors.New("curl: (22) The requested URL returned error: 404")
		}
	case "PUT":
		if !strings.Contains(path, "?") {
			if f.buckets == nil {
				f.buckets = map[string]bool{}
			}
			f.buckets[path] = true
		}
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
			Image: Image,
			State: "running",
			Ports: []docker.ContainerPort{
				{Private: internalPort, Public: 9000, Protocol: "tcp"},
			},
		},
	}
	var logBuf bytes.Buffer
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 9100, // mismatch
		Log: func(format string, args ...any) {
			logBuf.WriteString(format + "\n")
		},
	}

	id, err := ensureContainer(context.Background(), env, nil)
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
			Image: Image,
			State: "exited",
			Ports: []docker.ContainerPort{
				{Private: internalPort, Public: 9000, Protocol: "tcp"},
			},
		},
	}
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 9000,
		Log:           func(string, ...any) {},
	}
	id, err := ensureContainer(context.Background(), env, nil)
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

func TestEnsureContainer_StampsProxyLabelsWhenDomainsRequested(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 9000,
		Log:           func(string, ...any) {},
	}
	if _, err := ensureContainer(context.Background(), env, []string{"a.example.com", "b.example.com"}); err != nil {
		t.Fatalf("ensureContainer: %v", err)
	}
	if fd.lastSpec == nil {
		t.Fatal("expected ContainerCreate to be called")
	}
	got := fd.lastSpec.Labels["multienv.proxy.domain"]
	if got != "a.example.com,b.example.com" {
		t.Errorf("multienv.proxy.domain = %q, want %q", got, "a.example.com,b.example.com")
	}
	if fd.lastSpec.Labels["multienv.proxy.port"] != "9000" {
		t.Errorf("multienv.proxy.port = %q, want 9000", fd.lastSpec.Labels["multienv.proxy.port"])
	}
}

func TestEnsureContainer_OmitsProxyLabelsWhenNoDomains(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 9000,
		Log:           func(string, ...any) {},
	}
	if _, err := ensureContainer(context.Background(), env, nil); err != nil {
		t.Fatalf("ensureContainer: %v", err)
	}
	if _, has := fd.lastSpec.Labels["multienv.proxy.domain"]; has {
		t.Error("did not expect multienv.proxy.domain label when no domains requested")
	}
	if _, has := fd.lastSpec.Labels["multienv.proxy.port"]; has {
		t.Error("did not expect multienv.proxy.port label when no domains requested")
	}
}

func TestEnsureContainer_RecreatesOnDomainDrift(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		current: &docker.Container{
			ID:    "old-id",
			Names: []string{"/" + ContainerName},
			Image: Image,
			State: "running",
			Ports: []docker.ContainerPort{{Private: internalPort, Public: 9000, Protocol: "tcp"}},
			Labels: map[string]string{
				"multienv.proxy.domain": "old.example.com",
			},
		},
	}
	var logBuf bytes.Buffer
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 9000,
		Log:           func(format string, args ...any) { logBuf.WriteString(format + "\n") },
	}

	if _, err := ensureContainer(context.Background(), env, []string{"new.example.com"}); err != nil {
		t.Fatalf("ensureContainer: %v", err)
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
	if !strings.Contains(logBuf.String(), "proxy domains changed") {
		t.Errorf("expected proxy-domain-change log line, got: %s", logBuf.String())
	}
	if fd.lastSpec.Labels["multienv.proxy.domain"] != "new.example.com" {
		t.Errorf("new container's domain label = %q, want new.example.com",
			fd.lastSpec.Labels["multienv.proxy.domain"])
	}
}

func TestEnsureContainer_ReplacesLegacyMinIOContainer(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{
		current: &docker.Container{
			ID:    "old-id",
			Names: []string{"/" + ContainerName},
			Image: "minio/minio:latest",
			State: "running",
			Ports: []docker.ContainerPort{{Private: internalPort, Public: 9000, Protocol: "tcp"}},
		},
	}
	var logBuf bytes.Buffer
	env := accessory.Env{
		Docker:        fd,
		Network:       "multienv",
		HostBoundPort: 9000,
		Log:           func(format string, args ...any) { logBuf.WriteString(format + "\n") },
	}

	if _, err := ensureContainer(context.Background(), env, nil); err != nil {
		t.Fatalf("ensureContainer: %v", err)
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
	if !strings.Contains(logBuf.String(), "image changed") {
		t.Errorf("expected image-change log line, got: %s", logBuf.String())
	}
	if fd.lastSpec.Image != Image {
		t.Errorf("new container image = %q, want %q", fd.lastSpec.Image, Image)
	}
}

func TestEnsureBucket_CreatesWhenMissing(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{}
	env := accessory.Env{Docker: fd, Log: func(string, ...any) {}}
	created, err := ensureBucket(context.Background(), env, "id", "uploads")
	if err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	if !created {
		t.Error("expected created=true on first call")
	}
	wantSeq := []string{"HEAD /uploads", "PUT /uploads"}
	if !equalSeq(fd.calls, wantSeq) {
		t.Errorf("call sequence = %v, want %v", fd.calls, wantSeq)
	}
}

func TestEnsureBucket_NoOpWhenExists(t *testing.T) {
	t.Parallel()
	fd := &fakeDocker{buckets: map[string]bool{"/uploads": true}}
	env := accessory.Env{Docker: fd, Log: func(string, ...any) {}}
	created, err := ensureBucket(context.Background(), env, "id", "uploads")
	if err != nil {
		t.Fatalf("ensureBucket: %v", err)
	}
	if created {
		t.Error("expected created=false when bucket already present")
	}
	wantSeq := []string{"HEAD /uploads"}
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
		{Private: 9000, Public: 9000, Protocol: "tcp"},
	}}
	if !matchesHostPort(c, 9000) {
		t.Errorf("expected match on 9000")
	}
	if matchesHostPort(c, 9100) {
		t.Errorf("did not expect match on 9100")
	}
	if matchesHostPort(c, 0) {
		t.Errorf("did not expect match for 'off' when port is published")
	}
	empty := &docker.Container{}
	if !matchesHostPort(empty, 0) {
		t.Errorf("expected match for 'off' on unpublished container")
	}
}
