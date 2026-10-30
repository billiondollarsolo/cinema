package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
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
// platform ("linux/amd64") selects the image variant; always passed (P4-1: a
// tag pulled for one arch otherwise stands in for the other).
func (t *Task) ContainerRun(root, image, network string, cpus []int, platform string) (argv []string, stop func()) {
	name := "cinema-" + randomHex()
	back := fmt.Sprintf(`"$@"; rc=$?; chown -R %d:%d "$CINEMA_ROOT"; exit $rc`, os.Getuid(), os.Getgid())
	argv = append(append([]string{"env"}, DockerEnv()...), "docker", "run", "--rm", "--name", name, "--entrypoint", "sh",
		"-v", root+":"+root, "-w", filepath.Join(root, filepath.FromSlash(t.Cwd)),
		"-e", "CI=true", "-e", "CINEMA_ROOT="+root)
	if network != "" {
		argv = append(argv, "--network", network) // services by name, as in a container job
	}
	if platform != "" {
		argv = append(argv, "--platform", platform)
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
	return argv, func() { _ = docker("rm", "-f", name).Run() }
}

func randomHex() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// PinnedImageFor is PinnedImage for one platform: pulled for it if missing (a
// tag pulled for another arch must not stand in: P4-1), resolved to the
// registry digest. An image built locally has no registry digest; it's pinned
// by its local ID, which no other machine can match.
func PinnedImageFor(ref, platform string) (string, error) {
	if strings.Contains(ref, "@sha256:") {
		return ref, nil
	}
	inspect := func() ([]byte, error) {
		return docker("image", "inspect", "--platform", platform, "--format", "{{json .RepoDigests}} {{.Id}}", ref).Output()
	}
	out, err := inspect()
	if err != nil {
		if pout, perr := docker("pull", "-q", "--platform", platform, ref).CombinedOutput(); perr != nil {
			return "", fmt.Errorf("docker pull --platform %s: %v: %s", platform, perr, firstLine(string(pout)))
		}
		if out, err = inspect(); err != nil {
			return "", fmt.Errorf("docker image inspect: %v", err)
		}
	}
	digests, id, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	repo := ref
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo = ref[:i]
	}
	for _, rd := range strings.Split(strings.Trim(digests, "[]\""), "\",\"") {
		if r, d, ok := strings.Cut(rd, "@"); ok && (r == repo || strings.HasSuffix(r, "/"+repo)) {
			return repo + "@" + d, nil
		}
	}
	if strings.HasPrefix(id, "sha256:") {
		return id, nil
	}
	return "", fmt.Errorf("no digest for %s", ref)
}
