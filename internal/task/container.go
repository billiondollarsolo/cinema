package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var pinned sync.Map // image ref → repo@sha256:..., resolved once per process

// PinnedImage resolves ref to repo@sha256:digest, once per process, so the
// fingerprint and the run in one cinema invocation always see the same image
// even if the tag moves meanwhile.
func PinnedImage(ref string) (string, error) {
	if v, ok := pinned.Load(ref); ok {
		return v.(string), nil
	}
	d, err := imageDigest(ref)
	if err != nil {
		return "", err
	}
	repo, _, _ := strings.Cut(ref, "@")
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	v, _ := pinned.LoadOrStore(ref, repo+"@"+d)
	return v.(string), nil
}

// ContainerRun wraps t's argv to run in image (from PinnedImage), with the
// checkout at root mounted at the same path. It runs as root, like a GitHub
// container job, then hands the files back to this user so the checkout can be
// reset. stop removes the container if the client was killed (timeout, ^C).
// cpus (cpus:) becomes --cpuset-cpus: the container's processes are dockerd's
// children, so the CLI's own CPU affinity wouldn't reach them.
func (t *Task) ContainerRun(root, image, network string, cpus []int) (argv []string, stop func()) {
	name := "cinema-" + randomHex()
	back := fmt.Sprintf(`"$@"; rc=$?; chown -R %d:%d "$CINEMA_ROOT"; exit $rc`, os.Getuid(), os.Getgid())
	argv = []string{"docker", "run", "--rm", "--name", name, "--entrypoint", "sh",
		"-v", root + ":" + root, "-w", filepath.Join(root, filepath.FromSlash(t.Cwd)),
		"-e", "CI=true", "-e", "CINEMA_ROOT=" + root}
	if network != "" {
		argv = append(argv, "--network", network) // services by name, as in a container job
	}
	if len(cpus) > 0 {
		set := make([]string, len(cpus))
		for i, c := range cpus {
			set[i] = fmt.Sprint(c)
		}
		argv = append(argv, "--cpuset-cpus", strings.Join(set, ","))
	}
	argv = append(argv, image, "-c", back, "sh")
	argv = append(argv, t.Argv...)
	return argv, func() { _ = exec.Command("docker", "rm", "-f", name).Run() }
}

func randomHex() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}
