package task

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/billiondollarsolo/cinema/internal/store"
)

// cinema talks to the container engine with its own, empty client config, never
// the user's ~/.docker/config.json: that file can name a keychain credential
// helper (a password prompt on every pull, seen on a Mac in P4-1) and carries
// the user's registry logins, which a job's image pulls must not get (plan
// §9.1). The engine endpoint is kept: it comes from the user's current context
// (Colima, OrbStack, a remote DOCKER_HOST), which an empty config would lose.

var (
	dockerEnvOnce sync.Once
	dockerEnvVals []string
)

// DockerEnv is the environment cinema's docker CLI calls get on top of their own.
func DockerEnv() []string {
	dockerEnvOnce.Do(func() {
		host := os.Getenv("DOCKER_HOST")
		if host == "" {
			// The user's context, read with their config: inspecting a context
			// touches no credentials.
			if out, err := exec.Command("docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); err == nil {
				host = strings.TrimSpace(string(out))
			}
		}
		if host != "" {
			dockerEnvVals = append(dockerEnvVals, "DOCKER_HOST="+host)
		}
		if data, err := store.DataDir(); err == nil {
			dir := filepath.Join(data, "docker")
			if os.MkdirAll(dir, 0o700) == nil {
				cfg := filepath.Join(dir, "config.json")
				if _, err := os.Stat(cfg); err != nil {
					_ = os.WriteFile(cfg, []byte("{}\n"), 0o600)
				}
				dockerEnvVals = append(dockerEnvVals, "DOCKER_CONFIG="+dir)
			}
		}
	})
	return dockerEnvVals
}

// docker is exec.Command("docker", args...) with cinema's client env.
func docker(args ...string) *exec.Cmd {
	return dockerContext(context.Background(), args...)
}

func dockerContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = append(os.Environ(), DockerEnv()...)
	return cmd
}
