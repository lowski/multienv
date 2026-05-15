// Package docker isolates all interaction with the Docker daemon so the
// rest of multienv can stay free of the Docker SDK.
package docker

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"
)

// Client is a thin wrapper around the Docker SDK that exposes only the
// operations multienv needs.
type Client struct {
	api *client.Client
}

// New creates a Client using environment-driven configuration
// (DOCKER_HOST, DOCKER_API_VERSION, ...) with automatic API version
// negotiation against the daemon.
func New() (*Client, error) {
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{api: api}, nil
}

// Close releases the underlying Docker SDK resources.
func (c *Client) Close() error {
	return c.api.Close()
}

// ListContainers returns every container known to the daemon, including
// stopped ones, projected onto multienv's [Container] type.
func (c *Client) ListContainers(ctx context.Context) ([]Container, error) {
	result, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	out := make([]Container, 0, len(result.Items))
	for _, r := range result.Items {
		out = append(out, Container{
			ID:     r.ID,
			Names:  r.Names,
			Image:  r.Image,
			State:  string(r.State),
			Status: r.Status,
			Labels: r.Labels,
		})
	}
	return out, nil
}
