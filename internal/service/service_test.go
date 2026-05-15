package service

import (
	"reflect"
	"testing"

	"github.com/lowski/multienv/internal/docker"
)

func TestFromContainer_SkipsNonMultienv(t *testing.T) {
	t.Parallel()
	_, ok := FromContainer(docker.Container{
		Names:  []string{"/random"},
		Labels: map[string]string{"com.docker.compose.project": "x"},
	})
	if ok {
		t.Fatal("expected non-multienv container to be skipped")
	}
}

func TestFromContainer_UsesComposeMetadata(t *testing.T) {
	t.Parallel()
	got, ok := FromContainer(docker.Container{
		Names:  []string{"/myapp-api-1"},
		State:  "running",
		Status: "Up 3 hours",
		Labels: map[string]string{
			"com.docker.compose.project": "myapp",
			"com.docker.compose.service": "api",
			"multienv.proxy.domain":      "app.example.com",
			"multienv.postgres.db":       "myapp",
		},
	})
	if !ok {
		t.Fatal("expected service to be recognized")
	}
	want := Service{
		ContainerName: "myapp-api-1",
		Project:       "myapp",
		Name:          "api",
		State:         "running",
		Status:        "Up 3 hours",
		Accessories:   []string{"postgres", "proxy"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestFromContainer_FallsBackToContainerName(t *testing.T) {
	t.Parallel()
	got, ok := FromContainer(docker.Container{
		Names:  []string{"/lonely-service"},
		Labels: map[string]string{"multienv.cache": "true"},
	})
	if !ok {
		t.Fatal("expected service to be recognized")
	}
	if got.Name != "lonely-service" {
		t.Errorf("Name = %q, want %q", got.Name, "lonely-service")
	}
	if got.Project != "" {
		t.Errorf("Project = %q, want empty", got.Project)
	}
}

func TestFromContainers_FiltersAndPreservesOrder(t *testing.T) {
	t.Parallel()
	in := []docker.Container{
		{Names: []string{"/a"}, Labels: map[string]string{"multienv.x": "1"}},
		{Names: []string{"/b"}, Labels: map[string]string{"unrelated": "1"}},
		{Names: []string{"/c"}, Labels: map[string]string{"multienv.y": "1"}},
	}
	out := FromContainers(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Name != "a" || out[1].Name != "c" {
		t.Errorf("got names [%q %q], want [a c]", out[0].Name, out[1].Name)
	}
}
