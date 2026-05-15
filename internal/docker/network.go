package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// ErrNotFound is returned by lookup operations when the requested
// resource does not exist on the daemon.
var ErrNotFound = errors.New("docker: not found")

// Network is a minimal, daemon-agnostic view of a Docker network.
type Network struct {
	ID     string
	Name   string
	Labels map[string]string
	// AttachedContainerIDs is the set of container IDs currently
	// connected to the network.
	AttachedContainerIDs map[string]struct{}
}

// NetworkSpec describes a network to be created.
type NetworkSpec struct {
	Name       string
	Driver     string
	Attachable bool
	Labels     map[string]string
}

// NetworkInspect returns the network with the given name. If no such
// network exists it returns [ErrNotFound].
func (c *Client) NetworkInspect(ctx context.Context, name string) (*Network, error) {
	res, err := c.api.NetworkInspect(ctx, name, client.NetworkInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("inspect network %q: %w", name, err)
	}
	attached := make(map[string]struct{}, len(res.Network.Containers))
	for id := range res.Network.Containers {
		attached[id] = struct{}{}
	}
	return &Network{
		ID:                   res.Network.ID,
		Name:                 res.Network.Name,
		Labels:               res.Network.Labels,
		AttachedContainerIDs: attached,
	}, nil
}

// NetworkCreate creates a network from the given spec and returns the
// new network's ID.
func (c *Client) NetworkCreate(ctx context.Context, spec NetworkSpec) (string, error) {
	res, err := c.api.NetworkCreate(ctx, spec.Name, client.NetworkCreateOptions{
		Driver:     spec.Driver,
		Attachable: spec.Attachable,
		Labels:     spec.Labels,
	})
	if err != nil {
		return "", fmt.Errorf("create network %q: %w", spec.Name, err)
	}
	return res.ID, nil
}

// NetworkConnect attaches a container to a network.
func (c *Client) NetworkConnect(ctx context.Context, networkID, containerID string) error {
	if _, err := c.api.NetworkConnect(ctx, networkID, client.NetworkConnectOptions{
		Container: containerID,
	}); err != nil {
		return fmt.Errorf("connect %s to network %s: %w", containerID, networkID, err)
	}
	return nil
}
