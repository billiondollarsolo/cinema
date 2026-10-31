package analyze

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A symlinked directory pointing outside the repo: O_NOFOLLOW on the last
// component didn't stop it. Every reader of repo files goes through readRepo.
func TestRepoFilesStayInside(t *testing.T) {
	outside, root := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{".nvmrc": "22.23.4\n", "ci.yml": "leak:\n  image: alpine:3.20\n  script: [true]\n",
		"mise.toml": "[tools]\nnode = \"22.23.4\"\n", ".tool-versions": "nodejs 22.23.4\n", "global.json": `{"sdk": {"version": "9.0.100"}}`} {
		os.WriteFile(filepath.Join(outside, name), []byte(body), 0o644)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "real.txt"), []byte("in"), 0o644)
	os.Symlink("real.txt", filepath.Join(root, "alias.txt"))
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	if b, err := readRepo(root, "link/.nvmrc"); err == nil {
		t.Errorf("read through a symlinked directory: %q", b)
	}
	if b, err := readRepo(root, "../"+filepath.Base(outside)+"/.nvmrc"); err == nil {
		t.Errorf("read through ..: %q", b)
	}
	if b, err := readRepo(root, "alias.txt"); err != nil || string(b) != "in" {
		t.Errorf("a symlink inside the repo: %q %v", b, err) // as the runner's checkout would
	}
	if b, err := readRepoNoLinks(root, "alias.txt"); err == nil {
		t.Errorf("GitLab reads a symlink as the path it holds: %q", b)
	}
	done := make(chan error, 1)
	go func() { _, err := readRepo(root, "fifo"); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("read a FIFO")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hung on a FIFO")
	}

	if v := versionFromFile(root, "link/.nvmrc"); v != "" {
		t.Errorf("versionFromFile: %q", v)
	}
	if v, _ := javaVersionFile(root, "link/.nvmrc"); v != "" {
		t.Errorf("javaVersionFile: %q", v)
	}
	if spec, _ := globalJSONSDK(root, "link/global.json"); spec != "" {
		t.Errorf("globalJSONSDK: %q", spec)
	}
	if tools, _, why := miseConfig(root, "link", nil, miseFiles, false); why == "" || len(tools) > 0 {
		t.Errorf("mise working_directory through a symlink: %v %q", tools, why)
	}
	if GitLabExists(root, []string{"link/.nvmrc"}) == triTrue {
		t.Error("rules: exists: saw a file outside the repo")
	}
	// A local include through the symlinked directory: an error, never its jobs.
	jobs, err := GitLab(root, []byte("include: [{local: link/ci.yml}]\nunit:\n  image: alpine:3.20\n  script: [true]\n"))
	if err == nil {
		for _, j := range jobs {
			if j.Name == "leak" {
				t.Fatal("included a file outside the repo")
			}
		}
		t.Fatalf("no error: %v", jobs)
	} else if !strings.Contains(err.Error(), "link/ci.yml") {
		t.Errorf("error: %v", err)
	}
}
