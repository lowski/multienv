package proxy

import (
	"fmt"
	"strings"
)

// generateCaddyfile renders a Caddyfile that uses Caddy's internal
// local CA for all sites (no public ACME attempts) and reverse-proxies
// each domain to its declared upstream. HTTP→HTTPS redirection is
// handled by Caddy's auto_https default, which we leave on.
func generateCaddyfile(routes []route) string {
	var b strings.Builder
	b.WriteString("{\n\tlocal_certs\n}\n")
	for _, r := range routes {
		fmt.Fprintf(&b, "\n%s {\n\treverse_proxy %s\n}\n", r.Domain, r.Upstream)
	}
	return b.String()
}
