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
