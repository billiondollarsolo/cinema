package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// userConfig is the developer's own settings (never the repo's): which engine
// runs tasks, which runner image, which arches may be emulated (plan §9.5).
//
//	<user config dir>/cinema/config.yaml
//	engine: docker            # run Linux tasks in runner-like containers
//	image: ghcr.io/…/runner:ubuntu-24.04
//	emulate: [amd64]          # opt-in: run amd64 jobs under Rosetta/QEMU on an arm64 host
//
// CINEMA_ENGINE, CINEMA_RUNNER_IMAGE and CINEMA_EMULATE (comma-separated)
// override the file.
type userConfig struct {
	Engine  string   `yaml:"engine"`
	Image   string   `yaml:"image"`
	Emulate []string `yaml:"emulate"`
}

const defaultRunnerImage = "ghcr.io/billiondollarsolo/cinema-runner:ubuntu-24.04"

var (
	userCfgOnce sync.Once
	userCfg     userConfig
)

func loadUserConfig() userConfig {
	userCfgOnce.Do(func() {
		if dir, err := os.UserConfigDir(); err == nil {
			if b, err := os.ReadFile(filepath.Join(dir, "cinema", "config.yaml")); err == nil {
				_ = yaml.Unmarshal(b, &userCfg)
			}
		}
		if v, ok := os.LookupEnv("CINEMA_ENGINE"); ok {
			userCfg.Engine = v
		}
		if v := os.Getenv("CINEMA_RUNNER_IMAGE"); v != "" {
			userCfg.Image = v
		}
		if v, ok := os.LookupEnv("CINEMA_EMULATE"); ok {
			userCfg.Emulate = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
		}
		if userCfg.Image == "" {
			userCfg.Image = defaultRunnerImage
		}
	})
	return userCfg
}

var (
	engineArchOnce sync.Once
	engineArchVal  string
)

// engineArch is the arch the engine runs natively ("amd64", "arm64"), or "".
func engineArch() string {
	engineArchOnce.Do(func() {
		cmd := exec.Command("docker", "info", "--format", "{{.Architecture}}")
		cmd.Env = append(os.Environ(), task.DockerEnv()...)
		out, err := cmd.Output()
		if err != nil {
			return
		}
		engineArchVal = map[string]string{"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[strings.TrimSpace(string(out))]
	})
	return engineArchVal
}

// placement is where a task runs on this machine.
type placement struct {
	sandbox  bool   // in a runner-like container
	platform string // what it runs as: the task's platform, or the host's
	emulated bool
}

// place decides where t runs: natively on the host if the host is the task's
// platform and no engine is configured; otherwise in a sandbox if the engine
// runs that arch natively, or emulated when the user opted in. ok=false: not
// on this machine.
func place(t *task.Task) (p placement, ok bool) {
	cfg := loadUserConfig()
	if t.Container != "" && strings.HasPrefix(t.Platform, "linux/") {
		// A job container (container:) runs in the engine whatever the user's
		// engine setting; its platform is the image's, emulation opt-in as for
		// sandboxes.
		arch := strings.TrimPrefix(t.Platform, "linux/")
		switch native := engineArch(); {
		case native == arch:
			return placement{platform: t.Platform}, true
		case native != "" && slices.Contains(cfg.Emulate, arch):
			return placement{platform: t.Platform, emulated: true}, true
		}
		return placement{}, false
	}
	if t.Container == "" && cfg.Engine == "docker" && strings.HasPrefix(t.Platform, "linux/") {
		arch := strings.TrimPrefix(t.Platform, "linux/")
		switch native := engineArch(); {
		case native == arch:
			return placement{sandbox: true, platform: t.Platform}, true
		case native != "" && slices.Contains(cfg.Emulate, arch):
			return placement{sandbox: true, platform: t.Platform, emulated: true}, true
		}
	}
	if t.Platform == task.PlatformAny || t.Platform == task.Platform() {
		return placement{platform: task.Platform()}, true
	}
	return placement{}, false
}

// Probe sandboxes: for status, explain and push, a task that runs in a sandbox
// is fingerprinted in one (its platform, user and probes), shared per platform
// and runtime set for the command, checkout mounted read-only.
var (
	probeMu    sync.Mutex
	probeBoxes = map[string]*task.Sandbox{}
)

func probeWhere(r *repo, t *task.Task, pl placement) (*task.Where, error) {
	bins, err := t.RuntimeBins(r.dataDir, "linux", strings.TrimPrefix(pl.platform, "linux/"))
	if err != nil {
		return nil, err
	}
	cpus, mem := t.RunnerSize()
	key := fmt.Sprintf("%s|%s|%d|%d|%s", pl.platform, r.id, cpus, mem, strings.Join(bins, ":"))
	probeMu.Lock()
	defer probeMu.Unlock()
	if sb := probeBoxes[key]; sb != nil {
		return sb.Where(), nil
	}
	sb, err := task.StartSandbox(context.Background(), task.SandboxSpec{Image: loadUserConfig().Image, Platform: pl.platform,
		Emulated: pl.emulated, Root: r.root, RepoName: filepath.Base(r.id), RepoID: r.id, DataDir: r.dataDir, Bins: bins,
		CPUs: cpus, MemoryMB: mem * 1024,
		ProbeOnly: true, Lifetime: 30 * time.Minute})
	if err != nil {
		return nil, err
	}
	probeBoxes[key] = sb
	return sb.Where(), nil
}

// stopProbeSandboxes removes them; commands that fingerprint call it on return.
func stopProbeSandboxes() {
	probeMu.Lock()
	defer probeMu.Unlock()
	for k, sb := range probeBoxes {
		sb.Stop()
		delete(probeBoxes, k)
	}
}

// containerWhere fingerprints a container: task as what it is: the image's
// platform (not this host's), root, no probes (container tasks have none).
func containerWhere(t *task.Task, pl placement) *task.Where {
	cpus, mem := t.RunnerSize()
	return &task.Where{Platform: pl.platform, Root: true, CPUs: cpus, MemGB: mem, Inputs: []string{
		fmt.Sprintf("executor=container image=%s emulated=%t", t.Container, pl.emulated)}}
}

// placeHint says what would let this machine run t.
func placeHint(t *task.Task) string {
	cfg := loadUserConfig()
	arch := strings.TrimPrefix(t.Platform, "linux/")
	switch native := engineArch(); {
	case t.Container == "" && cfg.Engine != "docker":
		return "run it in a container: set engine: docker in your cinema config (or CINEMA_ENGINE=docker)"
	case native == "":
		return "no container engine answers (docker info failed)"
	default:
		return fmt.Sprintf("the engine runs %s natively; to run %s emulated (slower), add it to emulate: in your cinema config (or CINEMA_EMULATE=%s)", native, arch, arch)
	}
}
