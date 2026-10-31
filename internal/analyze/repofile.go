package analyze

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// maxRepoFile caps what the analyzer reads of a file the repo's CI config
// names.
const maxRepoFile = 1 << 20

// readRepo reads root/rel, a file the repo's CI config names, so untrusted:
// it must resolve inside root (os.Root refuses .. and symlinks that leave it,
// in any component, not just the last), be a regular file (a FIFO would hang
// the read, a device never end), and hold at most 1 MiB.
func readRepo(root, rel string) ([]byte, error) { return readIn(root, rel, true) }

// readRepoNoLinks is readRepo for files GitLab reads from git, not from a
// checkout (.gitlab-ci.yml and its local includes): there a symlink is a
// blob holding a path, never what it points to, so no component may be one.
func readRepoNoLinks(root, rel string) ([]byte, error) { return readIn(root, rel, false) }

func readIn(root, rel string, links bool) ([]byte, error) {
	if root == "" {
		return nil, errors.New("no repository to read it from")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	rel = filepath.Clean(filepath.FromSlash(rel))
	if !links {
		parts := strings.Split(rel, string(filepath.Separator))
		for i := range parts {
			fi, err := r.Lstat(filepath.Join(parts[:i+1]...))
			if err != nil {
				return nil, err
			}
			if fi.Mode()&fs.ModeSymlink != 0 {
				return nil, errors.New("a symlink: GitLab reads it from git, as the path it holds")
			}
		}
	}
	f, err := r.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRepoFile+1))
	if err == nil && len(b) > maxRepoFile {
		err = errors.New("larger than 1 MiB")
	}
	return b, err
}

// readSmall is readRepo's content, nil if it can't be read.
func readSmall(root, rel string) []byte {
	b, err := readRepo(root, rel)
	if err != nil {
		return nil
	}
	return b
}

// statRepo is Stat of root/rel resolved inside root like readRepo, so an
// existence check can't look outside the repo either: a path that leaves it
// is an error other than fs.ErrNotExist. lstatRepo doesn't follow a final
// symlink (GitLab's exists: sees git's tree entries).
func statRepo(root, rel string) (fs.FileInfo, error) { return statIn(root, rel, (*os.Root).Stat) }

func lstatRepo(root, rel string) (fs.FileInfo, error) { return statIn(root, rel, (*os.Root).Lstat) }

func statIn(root, rel string, stat func(*os.Root, string) (fs.FileInfo, error)) (fs.FileInfo, error) {
	if root == "" {
		return nil, errors.New("no repository")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return stat(r, filepath.FromSlash(rel))
}
