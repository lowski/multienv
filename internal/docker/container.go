package docker

// Container is a minimal, daemon-agnostic view of a Docker container.
// It carries only the fields multienv needs so callers do not depend on
// the Docker SDK types.
type Container struct {
	ID     string
	Names  []string
	Image  string
	State  string
	Status string
	Labels map[string]string
	Ports  []ContainerPort
}

// ContainerPort describes a single container-side port and, if mapped,
// its host-side publication.
type ContainerPort struct {
	Private  uint16 // the port the container exposes
	Public   uint16 // 0 if not published to the host
	Protocol string // "tcp", "udp"
}

// PrimaryName returns the first container name with any leading slash
// stripped, or an empty string if no names are set.
func (c Container) PrimaryName() string {
	if len(c.Names) == 0 {
		return ""
	}
	name := c.Names[0]
	if len(name) > 0 && name[0] == '/' {
		return name[1:]
	}
	return name
}

// ContainerSpec describes the desired shape of a container to create.
type ContainerSpec struct {
	Name          string
	Image         string
	Cmd           []string
	Env           []string
	Networks      []string // network names to attach at create time
	HostBindings  []PortBinding
	Mounts        []Mount
	Labels        map[string]string
	RestartPolicy string // "no", "always", "unless-stopped", "on-failure"
}

// PortBinding maps a host port to a container port.
type PortBinding struct {
	HostIP        string // "0.0.0.0", "127.0.0.1"
	HostPort      uint16
	ContainerPort uint16
	Protocol      string // "tcp", "udp"
}

// Mount describes a volume or bind mount.
type Mount struct {
	Type   string // "volume", "bind"
	Source string // volume name or host path
	Target string // path inside the container
}
