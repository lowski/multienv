// Package service models the project-managed containers that multienv
// recognizes by their Docker labels.
package service

import (
	"github.com/lowski/multienv/internal/docker"
	"github.com/lowski/multienv/internal/labels"
)

// Service is a container that opts in to multienv by carrying at least
// one label under the multienv prefix.
type Service struct {
	// ContainerID is the underlying Docker container ID.
	ContainerID string

	// Project is the Docker Compose project the container belongs to,
	// or empty if the container is not part of a compose project.
	Project string

	// Name is the Compose service name, falling back to the container's
	// primary name when no compose label is set.
	Name string

	// State is the machine-readable container state (running, exited, ...).
	State string

	// Status is the human-readable status string from the daemon.
	Status string

	// Accessories are the multienv accessory types this service requests
	// via its labels, sorted alphabetically.
	Accessories []string
}

// FromContainer derives a Service from a container. The second return
// value is false when the container is not a multienv service.
func FromContainer(c docker.Container) (Service, bool) {
	if !labels.HasMultienv(c.Labels) {
		return Service{}, false
	}
	return Service{
		ContainerID: c.ID,
		Project:     c.Labels[labels.ComposeProject],
		Name:        serviceName(c),
		State:       c.State,
		Status:      c.Status,
		Accessories: labels.Accessories(c.Labels),
	}, true
}

// FromContainers filters and maps a slice of containers, returning only
// the ones that qualify as multienv services.
func FromContainers(cs []docker.Container) []Service {
	out := make([]Service, 0, len(cs))
	for _, c := range cs {
		if s, ok := FromContainer(c); ok {
			out = append(out, s)
		}
	}
	return out
}

func serviceName(c docker.Container) string {
	if v, ok := c.Labels[labels.ComposeService]; ok && v != "" {
		return v
	}
	return c.PrimaryName()
}
