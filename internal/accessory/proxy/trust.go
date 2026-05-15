package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/lowski/multienv/internal/accessory"
	"github.com/lowski/multienv/internal/docker"
)

// trustCA extracts Caddy's internal CA root certificate from the proxy
// container, writes it to a temp path on the host, and prints
// OS-appropriate instructions to add it to the system trust store. It
// deliberately does not run sudo itself.
func (Accessory) trustCA(ctx context.Context, env accessory.Env, _ []string) error {
	if _, err := env.Docker.ContainerInspect(ctx, ContainerName); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return errors.New("proxy container is not running; declare multienv.proxy.domain on a service and run `multienv reconcile` first")
		}
		return err
	}

	dest := filepath.Join(os.TempDir(), "multienv-caddy-ca.crt")
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer f.Close()

	if err := env.Docker.ContainerCopyFrom(ctx, ContainerName, caRootPath, f); err != nil {
		if errors.Is(err, docker.ErrNotFound) {
			return errors.New("Caddy CA cert not found yet; let the proxy issue at least one cert (visit your domain in a browser once) and retry")
		}
		return fmt.Errorf("extract CA: %w", err)
	}

	env.Log("CA cert written to %s", dest)
	env.Log("")
	switch runtime.GOOS {
	case "darwin":
		env.Log("To trust on macOS:")
		env.Log("  sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain %s", dest)
	case "linux":
		env.Log("To trust on Linux (Debian/Ubuntu):")
		env.Log("  sudo cp %s /usr/local/share/ca-certificates/multienv-caddy.crt", dest)
		env.Log("  sudo update-ca-certificates")
	default:
		env.Log("Add %s to your system's trusted root store.", dest)
	}
	return nil
}
