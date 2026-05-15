// Package labels defines the Docker label namespace multienv uses to
// recognize and configure services.
package labels

import (
	"sort"
	"strings"
)

const (
	// Prefix is the root namespace for all multienv labels.
	Prefix = "multienv."

	// ComposeProject is the standard Docker Compose project label.
	ComposeProject = "com.docker.compose.project"

	// ComposeService is the standard Docker Compose service label.
	ComposeService = "com.docker.compose.service"
)

// HasMultienv reports whether any label key starts with the multienv prefix.
func HasMultienv(m map[string]string) bool {
	for k := range m {
		if strings.HasPrefix(k, Prefix) {
			return true
		}
	}
	return false
}

// Accessories returns the unique accessory types referenced by multienv
// labels, sorted alphabetically. The accessory type is the first segment
// after the prefix: "multienv.proxy.domain" -> "proxy".
func Accessories(m map[string]string) []string {
	seen := map[string]struct{}{}
	for k := range m {
		rest, ok := strings.CutPrefix(k, Prefix)
		if !ok || rest == "" {
			continue
		}
		name, _, _ := strings.Cut(rest, ".")
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
