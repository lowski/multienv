package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"path"
	"strconv"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// ContainerInspect returns the container identified by name or ID. If
// no such container exists it returns [ErrNotFound].
func (c *Client) ContainerInspect(ctx context.Context, nameOrID string) (*Container, error) {
	res, err := c.api.ContainerInspect(ctx, nameOrID, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("inspect container %q: %w", nameOrID, err)
	}
	r := res.Container
	out := &Container{
		ID:     r.ID,
		Image:  r.Image,
		Labels: r.Config.Labels,
	}
	if r.Name != "" {
		out.Names = []string{r.Name}
	}
	if r.State != nil {
		out.State = string(r.State.Status)
	}
	out.Ports = readInspectPorts(r)
	return out, nil
}

// readInspectPorts merges image-exposed ports with the live host-port
// publications recorded in NetworkSettings.Ports. ExposedPorts alone
// only tells you which container ports an image declares; the
// publication-to-host mapping lives in NetworkSettings.
func readInspectPorts(r container.InspectResponse) []ContainerPort {
	published := map[network.Port]uint16{}
	if r.NetworkSettings != nil {
		for port, bindings := range r.NetworkSettings.Ports {
			for _, b := range bindings {
				if b.HostPort == "" {
					continue
				}
				n, err := strconv.ParseUint(b.HostPort, 10, 16)
				if err != nil {
					continue
				}
				published[port] = uint16(n)
				break
			}
		}
	}

	seen := map[network.Port]bool{}
	var out []ContainerPort
	add := func(p network.Port) {
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, ContainerPort{
			Private:  p.Num(),
			Public:   published[p],
			Protocol: string(p.Proto()),
		})
	}
	if r.Config != nil {
		for p := range r.Config.ExposedPorts {
			add(p)
		}
	}
	if r.NetworkSettings != nil {
		for p := range r.NetworkSettings.Ports {
			add(p)
		}
	}
	return out
}

// ContainerCreate creates a container from the given spec and returns
// its ID. It does not start the container.
func (c *Client) ContainerCreate(ctx context.Context, spec ContainerSpec) (string, error) {
	cfg := &container.Config{
		Image:  spec.Image,
		Cmd:    spec.Cmd,
		Env:    spec.Env,
		Labels: spec.Labels,
	}

	hostCfg := &container.HostConfig{}
	if spec.RestartPolicy != "" {
		hostCfg.RestartPolicy = container.RestartPolicy{Name: container.RestartPolicyMode(spec.RestartPolicy)}
	}

	if len(spec.HostBindings) > 0 {
		cfg.ExposedPorts = network.PortSet{}
		hostCfg.PortBindings = network.PortMap{}
		for _, b := range spec.HostBindings {
			proto := b.Protocol
			if proto == "" {
				proto = "tcp"
			}
			port, err := network.ParsePort(fmt.Sprintf("%d/%s", b.ContainerPort, proto))
			if err != nil {
				return "", fmt.Errorf("invalid port %d/%s: %w", b.ContainerPort, proto, err)
			}
			cfg.ExposedPorts[port] = struct{}{}
			hostIP, err := netip.ParseAddr(b.HostIP)
			if err != nil {
				return "", fmt.Errorf("invalid host IP %q: %w", b.HostIP, err)
			}
			hostCfg.PortBindings[port] = append(hostCfg.PortBindings[port], network.PortBinding{
				HostIP:   hostIP,
				HostPort: fmt.Sprintf("%d", b.HostPort),
			})
		}
	}

	for _, m := range spec.Mounts {
		hostCfg.Binds = append(hostCfg.Binds, m.Source+":"+m.Target)
	}

	var netCfg *network.NetworkingConfig
	if len(spec.Networks) > 0 {
		netCfg = &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{},
		}
		for _, n := range spec.Networks {
			netCfg.EndpointsConfig[n] = &network.EndpointSettings{}
		}
	}

	res, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             spec.Name,
		Config:           cfg,
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return "", fmt.Errorf("create container %q: %w", spec.Name, err)
	}
	return res.ID, nil
}

// ContainerStart starts a previously-created container.
func (c *Client) ContainerStart(ctx context.Context, id string) error {
	if _, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start container %s: %w", id, err)
	}
	return nil
}

// ContainerRemove deletes a container. When force is true the
// container is killed first if running. Named volumes attached to the
// container are not removed (only the container itself).
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	if _, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: force}); err != nil {
		return fmt.Errorf("remove container %s: %w", id, err)
	}
	return nil
}

// ContainerExec runs cmd inside the container. stdin, if non-nil, is
// piped to the process; stdout/stderr are demultiplexed — stdout to
// stdout (if non-nil), stderr is buffered and surfaced in the error
// when the exit code is non-zero.
func (c *Client) ContainerExec(ctx context.Context, id string, cmd []string, stdin io.Reader, stdout io.Writer) error {
	exec, err := c.api.ExecCreate(ctx, id, client.ExecCreateOptions{
		AttachStdin:  stdin != nil,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return fmt.Errorf("exec create: %w", err)
	}

	att, err := c.api.ExecAttach(ctx, exec.ID, client.ExecAttachOptions{})
	if err != nil {
		return fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	stdinErr := make(chan error, 1)
	go func() {
		if stdin != nil {
			_, err := io.Copy(att.Conn, stdin)
			_ = att.CloseWrite()
			stdinErr <- err
			return
		}
		stdinErr <- nil
	}()

	if stdout == nil {
		stdout = io.Discard
	}
	var stderrBuf bytes.Buffer
	if err := demuxDockerStream(att.Reader, stdout, &stderrBuf); err != nil {
		return fmt.Errorf("exec read: %w", err)
	}
	if err := <-stdinErr; err != nil {
		return fmt.Errorf("exec stdin: %w", err)
	}

	insp, err := c.api.ExecInspect(ctx, exec.ID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("exec inspect: %w", err)
	}
	if insp.ExitCode != 0 {
		return fmt.Errorf("exec exit %d: %s", insp.ExitCode, bytes.TrimSpace(stderrBuf.Bytes()))
	}
	return nil
}

// ContainerCopyFrom extracts the single file at srcPath inside the
// container and writes its contents to dst. Directory copies are not
// supported.
func (c *Client) ContainerCopyFrom(ctx context.Context, id, srcPath string, dst io.Writer) error {
	res, err := c.api.CopyFromContainer(ctx, id, client.CopyFromContainerOptions{SourcePath: srcPath})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return ErrNotFound
		}
		return fmt.Errorf("copy from container: %w", err)
	}
	defer res.Content.Close()

	tr := tar.NewReader(res.Content)
	want := path.Base(srcPath)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("file %q not found in archive", srcPath)
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if path.Base(hdr.Name) != want || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if _, err := io.Copy(dst, tr); err != nil {
			return fmt.Errorf("write file: %w", err)
		}
		return nil
	}
}

// ImagePull pulls ref from its registry and waits for the pull to
// complete. It is safe to call when the image is already present.
func (c *Client) ImagePull(ctx context.Context, ref string) error {
	resp, err := c.api.ImagePull(ctx, ref, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer resp.Close()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// demuxDockerStream parses Docker's multiplexed exec/attach stream and
// routes stdout/stderr to the respective writers.
//
// Frame format: 8-byte header — [stream, 0, 0, 0, sizeBE32] — followed
// by size bytes of payload. stream is 1 for stdout, 2 for stderr.
func demuxDockerStream(r io.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	for {
		_, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		size := binary.BigEndian.Uint32(hdr[4:])
		dst := stdout
		if hdr[0] == 2 {
			dst = stderr
		}
		if _, err := io.CopyN(dst, r, int64(size)); err != nil {
			return err
		}
	}
}
