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

// ForAccessory extracts the labels under "multienv.<name>." with the
// shared prefix stripped from the keys. A label of "multienv.proxy.domain"
// becomes "domain" in the returned map. Returns nil when no matching
// labels exist.
func ForAccessory(m map[string]string, name string) map[string]string {
	prefix := Prefix + name + "."
	var out map[string]string
	for k, v := range m {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok || rest == "" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[rest] = v
	}
	return out
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
