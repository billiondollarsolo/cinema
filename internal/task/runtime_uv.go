package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// uvDist is uv's GitHub release downloads (a variable so tests can point it at a fake).
var uvDist = "https://github.com/astral-sh/uv/releases/download"

func init() {
	RegisterRuntime(&RuntimeKind{Name: "uv", Example: "0.9.5", Version: regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`),
		Provision: provisionUv, Bins: []string{"bin"},
		// uv's cache is ~/.cache/uv: already on the sandbox's cache volume.
		Probe: []string{"uv", "--version"}})
}

// provisionUv installs uv ver (uv and uvx) under dataDir/runtimes/uv, from the
// release tarball checked against the .sha256 file published beside it.
func provisionUv(dataDir, ver, osName, arch string) (string, error) {
	tr, err := triple(osName, arch)
	if err != nil {
		return "", err
	}
	root := filepath.Join(dataDir, "runtimes", "uv")
	dest := filepath.Join(root, "uv-"+ver+"-"+tr)
	if fi, err := os.Stat(filepath.Join(dest, "bin", "uv")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now) // last use, for the cleanup sweep
		return dest, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	file := "uv-" + tr + ".tar.gz"
	sums, err := httpGet(client, uvDist+"/"+ver+"/"+file+".sha256", 4<<10)
	if err != nil {
		return "", err
	}
	want := sumFor(sums, file)
	if want == "" {
		return "", fmt.Errorf("%s.sha256 doesn't list %s", file, file)
	}
	tarball, err := httpGet(client, uvDist+"/"+ver+"/"+file, 200<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(tarball); hex.EncodeToString(h[:]) != want {
		return "", errors.New("checksum mismatch")
	}
	return installTarGz(root, "uv-"+ver+"-"+tr, tarball, "uv-"+tr+"/", "bin", "bin/uv", nil)
}
