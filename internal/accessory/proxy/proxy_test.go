package proxy

import (
	"strings"
	"testing"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/service"
)

func req(name string, cfg map[string]string, ports ...uint16) accessory.ServiceRequest {
	cp := make([]docker.ContainerPort, len(ports))
	for i, p := range ports {
		cp[i] = docker.ContainerPort{Private: p, Protocol: "tcp"}
	}
	return accessory.ServiceRequest{
		Service: service.Service{
			ContainerName: name,
			Name:          name,
			Project:       "myapp",
			Ports:         cp,
		},
		Config: cfg,
	}
}

func TestResolvePort_LabelTakesPrecedence(t *testing.T) {
	t.Parallel()
	r := req("api", map[string]string{"port": "8080"}, 3000, 9000)
	got, err := resolvePort(r)
	if err != nil {
		t.Fatalf("resolvePort: %v", err)
	}
	if got != 8080 {
		t.Errorf("port = %d, want 8080", got)
	}
}

func TestResolvePort_FallsBackToSmallestExposed(t *testing.T) {
	t.Parallel()
	r := req("api", nil, 9000, 3000, 4000)
	got, err := resolvePort(r)
	if err != nil {
		t.Fatalf("resolvePort: %v", err)
	}
	if got != 3000 {
		t.Errorf("port = %d, want 3000 (smallest exposed)", got)
	}
}

func TestResolvePort_ErrorsWhenNothingAvailable(t *testing.T) {
	t.Parallel()
	r := req("api", nil) // no label, no ports
	if _, err := resolvePort(r); err == nil {
		t.Fatal("expected error when no port info available")
	}
}

func TestResolvePort_InvalidLabel(t *testing.T) {
	t.Parallel()
	r := req("api", map[string]string{"port": "not-a-number"}, 3000)
	if _, err := resolvePort(r); err == nil {
		t.Fatal("expected error on invalid port label")
	}
}

func TestBuildRoutes_HappyPathSortsByDomain(t *testing.T) {
	t.Parallel()
	in := []accessory.ServiceRequest{
		req("web", map[string]string{"domain": "z.example.com"}, 8080),
		req("api", map[string]string{"domain": "a.example.com", "port": "3000"}, 3000),
	}
	routes, errs := buildRoutes(in)
	if len(errs) != 0 {
		t.Fatalf("unexpected per-service errors: %v", errs)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	if routes[0].Domain != "a.example.com" || routes[1].Domain != "z.example.com" {
		t.Errorf("routes not sorted: %+v", routes)
	}
	if routes[0].Upstream != "api:3000" {
		t.Errorf("upstream = %q, want api:3000", routes[0].Upstream)
	}
	if routes[1].Upstream != "web:8080" {
		t.Errorf("upstream = %q, want web:8080", routes[1].Upstream)
	}
}

func TestBuildRoutes_SkipsBadServicesAndReportsThem(t *testing.T) {
	t.Parallel()
	in := []accessory.ServiceRequest{
		req("ok", map[string]string{"domain": "ok.example.com"}, 3000),
		req("nodomain", map[string]string{"port": "3000"}, 3000),
		req("noport", map[string]string{"domain": "noport.example.com"}), // no label, no exposed ports
	}
	routes, errs := buildRoutes(in)
	if len(routes) != 1 || routes[0].Domain != "ok.example.com" {
		t.Errorf("routes = %+v, want only ok.example.com", routes)
	}
	if _, ok := errs["myapp/nodomain"]; !ok {
		t.Errorf("expected error for myapp/nodomain in %v", errs)
	}
	if _, ok := errs["myapp/noport"]; !ok {
		t.Errorf("expected error for myapp/noport in %v", errs)
	}
}

func TestBuildRoutes_CommaSeparatedDomainsFanOut(t *testing.T) {
	t.Parallel()
	in := []accessory.ServiceRequest{
		req("s3", map[string]string{"domain": "a.example.com, b.example.com", "port": "9000"}, 9000),
	}
	routes, errs := buildRoutes(in)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(routes) != 2 {
		t.Fatalf("got %d routes, want 2", len(routes))
	}
	if routes[0].Domain != "a.example.com" || routes[1].Domain != "b.example.com" {
		t.Errorf("routes not as expected: %+v", routes)
	}
	if routes[0].Upstream != "s3:9000" || routes[1].Upstream != "s3:9000" {
		t.Errorf("both routes should share the upstream s3:9000, got %+v", routes)
	}
}

func TestGenerateCaddyfile_Shape(t *testing.T) {
	t.Parallel()
	got := generateCaddyfile([]route{
		{Domain: "a.example.com", Upstream: "api:3000"},
		{Domain: "b.example.com", Upstream: "web:8080"},
	})
	if !strings.Contains(got, "local_certs") {
		t.Error("missing local_certs directive")
	}
	if !strings.Contains(got, "a.example.com {\n\treverse_proxy api:3000\n}") {
		t.Errorf("missing route for a.example.com:\n%s", got)
	}
	if !strings.Contains(got, "b.example.com {\n\treverse_proxy web:8080\n}") {
		t.Errorf("missing route for b.example.com:\n%s", got)
	}
}

func TestConfigSchema_DocumentsRequiredAndOptional(t *testing.T) {
	t.Parallel()
	s := Accessory{}.ConfigSchema()
	if !s["domain"].Required {
		t.Error("domain must be marked required")
	}
	if s["port"].Required {
		t.Error("port must be optional")
	}
	if s["port"].Default == "" {
		t.Error("port should document its default behavior")
	}
}
