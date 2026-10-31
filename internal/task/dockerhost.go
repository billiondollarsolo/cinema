package task

import (
	"net"
	"runtime"
	"strings"
)

// docker: host (plan §9.2, ADR 0003): the task may use the engine through
// its socket, as a job on GitHub's runner uses the runner's Docker
// (Testcontainers, docker run/build/compose). Root-equivalent on the engine,
// so off by default and part of the contract.

// engineSocket is the engine's socket as a path on the engine's own host,
// where bind mounts resolve: a local Linux engine's unix DOCKER_HOST path
// (rootless: /run/user/<uid>/docker.sock), else /var/run/docker.sock, which
// Colima's, Docker Desktop's and OrbStack's VMs provide.
func engineSocket() string {
	if runtime.GOOS == "linux" && !strings.Contains(EngineServer().OS, "Docker Desktop") {
		for _, kv := range DockerEnv() {
			if p, ok := strings.CutPrefix(kv, "DOCKER_HOST=unix://"); ok && p != "" {
				return p
			}
		}
	}
	return "/var/run/docker.sock"
}

// dockerHostEnv is what a docker: host task gets: the socket as the default
// endpoint, the engine-side socket path for containers that mount it
// themselves (Testcontainers' Ryuk reaper), and where the ports of the
// containers it starts are: on a runner that's localhost; here it's the
// engine's host, the sandbox's gateway. Testcontainers finds that itself in a
// container; Spring Boot's Docker Compose support reads SERVICES_HOST (lab:
// petclinic's Postgres tests waited on localhost and timed out without it).
// A test that hardcodes localhost still fails here: a red, never a green.
func dockerHostEnv(gateway string) []string {
	return []string{"DOCKER_HOST=unix:///var/run/docker.sock", "TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=" + engineSocket(),
		"TESTCONTAINERS_HOST_OVERRIDE=" + gateway, "SERVICES_HOST=" + gateway}
}

// defaultGateway reads `ip route show default`: "default via 172.17.0.1 dev eth0".
func defaultGateway(route string) string {
	f := strings.Fields(route)
	for i := 0; i+2 < len(f); i++ {
		if f[i] == "default" && f[i+1] == "via" && net.ParseIP(f[i+2]) != nil {
			return f[i+2]
		}
	}
	return ""
}

// EngineObjects are the engine's containers and networks at one moment.
type EngineObjects struct{ containers, networks map[string]bool }

// SnapshotEngine lists the engine's containers and networks, before a
// docker: host task runs.
func SnapshotEngine() EngineObjects {
	return EngineObjects{containers: engineIDs("ps", "-aq", "--no-trunc"), networks: engineIDs("network", "ls", "-q", "--no-trunc")}
}

func engineIDs(args ...string) map[string]bool {
	out, err := docker(args...).Output()
	m := map[string]bool{}
	if err != nil {
		return nil
	}
	for _, id := range strings.Fields(string(out)) {
		m[id] = true
	}
	return m
}

// RemoveNew removes the containers and networks created since before that
// cinema didn't create itself (its own are named cinema-…), and returns their
// names. A docker: host task runs alone (and holds the engine lock), so what
// appeared meanwhile is the task's: containers its tests left running, a
// compose project never taken down. Testcontainers' reaper usually got there
// first. ponytail: volumes the task created are left (a volume you made
// meanwhile would be indistinguishable).
func (before EngineObjects) RemoveNew() []string {
	if before.containers == nil || before.networks == nil {
		return nil // couldn't list before: remove nothing rather than guess
	}
	var removed []string
	for id := range engineIDs("ps", "-aq", "--no-trunc") {
		if before.containers[id] {
			continue
		}
		name := inspectName(id, "{{.Name}}")
		if strings.HasPrefix(name, "cinema-") {
			continue
		}
		if docker("rm", "-f", "-v", id).Run() == nil {
			removed = append(removed, "container "+name)
		}
	}
	for id := range engineIDs("network", "ls", "-q", "--no-trunc") {
		if before.networks[id] {
			continue
		}
		name := inspectName(id, "{{.Name}}")
		if strings.HasPrefix(name, "cinema-") {
			continue
		}
		if docker("network", "rm", id).Run() == nil {
			removed = append(removed, "network "+name)
		}
	}
	return removed
}

func inspectName(id, format string) string {
	out, err := docker("inspect", "--format", format, id).Output()
	if err != nil {
		return id[:min(len(id), 12)]
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "/")
}
