package proxy

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
)

// resolvePort picks the target port for one service. Priority:
// 1. multienv.proxy.port label
// 2. smallest container-exposed port
func resolvePort(r accessory.ServiceRequest) (uint16, error) {
	if v, ok := r.Config["port"]; ok && v != "" {
		n, err := strconv.ParseUint(v, 10, 16)
		if err != nil {
			return 0, fmt.Errorf("invalid multienv.proxy.port %q: %w", v, err)
		}
		return uint16(n), nil
	}
	if len(r.Service.Ports) == 0 {
		return 0, errors.New("no multienv.proxy.port label and container exposes no ports")
	}
	sorted := make([]docker.ContainerPort, len(r.Service.Ports))
	copy(sorted, r.Service.Ports)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Private < sorted[j].Private })
	return sorted[0].Private, nil
}
