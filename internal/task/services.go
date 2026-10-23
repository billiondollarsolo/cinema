package task

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Service is a sidecar container a task needs, shaped like a GitHub job's
// services: entry. It's reachable on localhost:<host port> (ports:) and, from a
// container: task, by its name.
type Service struct {
	Image   string            `yaml:"image"`
	Env     map[string]string `yaml:"env"`
	Ports   []string          `yaml:"ports"`
	Options string            `yaml:"options"` // docker run flags, as GitHub passes them (e.g. --health-cmd)
}

var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)

func validateServices(s map[string]Service) error {
	for name, svc := range s {
		if !serviceName.MatchString(name) {
			return fmt.Errorf("service name %q: use lowercase letters, digits, . _ -", name)
		}
		if svc.Image == "" {
			return fmt.Errorf("service %s: image is required", name)
		}
		if _, err := shellWords(svc.Options); err != nil {
			return fmt.Errorf("service %s: options: %v", name, err)
		}
	}
	return nil
}

func sortedServices(s map[string]Service) []string {
	names := make([]string, 0, len(s))
	for n := range s {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// StartServices starts t's services on a private network and waits until each
// is healthy (its health check passes) or, without one, running. stop removes
// them and the network. Images run by the digest the fingerprint records.
func (t *Task) StartServices() (network string, stop func(), err error) {
	if len(t.Services) == 0 {
		return "", func() {}, nil
	}
	network = "cinema-" + randomHex()
	var started []string
	stop = func() {
		for _, c := range started {
			_ = exec.Command("docker", "rm", "-f", c).Run()
		}
		_ = exec.Command("docker", "network", "rm", network).Run()
	}
	if out, err := exec.Command("docker", "network", "create", network).CombinedOutput(); err != nil {
		return "", func() {}, fmt.Errorf("docker network create: %v: %s", err, firstLine(string(out)))
	}
	for _, name := range sortedServices(t.Services) {
		svc := t.Services[name]
		img, err := PinnedImage(svc.Image)
		if err != nil {
			stop()
			return "", func() {}, fmt.Errorf("service %s: %w", name, err)
		}
		c := network + "-" + name
		args := []string{"run", "-d", "--name", c, "--network", network, "--network-alias", name}
		for _, k := range sortedKeys(svc.Env) {
			args = append(args, "-e", k+"="+svc.Env[k])
		}
		for _, p := range svc.Ports {
			args = append(args, "-p", p)
		}
		opts, _ := shellWords(svc.Options)
		args = append(append(args, opts...), img)
		if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
			stop()
			return "", func() {}, fmt.Errorf("service %s: docker run: %v: %s", name, err, firstLine(string(out)))
		}
		started = append(started, c)
	}
	for _, c := range started {
		if err := waitHealthy(c, 3*time.Minute); err != nil {
			stop()
			return "", func() {}, err
		}
	}
	return network, stop, nil
}

func waitHealthy(c string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		out, err := exec.Command("docker", "inspect", "-f", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", c).Output()
		state := strings.TrimSpace(string(out))
		switch {
		case err != nil:
			return fmt.Errorf("service %s: docker inspect: %v", c, err)
		case state == "healthy", state == "running":
			return nil
		case state == "unhealthy", state == "exited", state == "dead":
			return fmt.Errorf("service %s is %s", c, state)
		case time.Now().After(deadline):
			return fmt.Errorf("service %s not healthy after %s (%s)", c, limit, state)
		}
		time.Sleep(time.Second)
	}
}

// shellWords splits docker options the way a shell would for simple quoting:
// GitHub's options often quote a health command ("--health-cmd 'pg_isready -U x'").
func shellWords(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	in, quote, esc := false, rune(0), false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc, in = false, true
		case r == '\\' && quote != '\'':
			esc = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if in {
				out, in = append(out, cur.String()), false
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 || esc {
		return nil, errors.New("unterminated quote")
	}
	if in {
		out = append(out, cur.String())
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
