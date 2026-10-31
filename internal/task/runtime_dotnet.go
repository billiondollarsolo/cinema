package task

import (
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// dotnetDist is Microsoft's .NET download host, for release metadata and SDK
// archives alike (a variable so tests can point it at a fake).
var dotnetDist = "https://builds.dotnet.microsoft.com/dotnet"

// An SDK version: its patch is a feature band and more (8.0.404). A runtime
// version (8.0.11) is a different thing and must not pass for one.
var dotnetVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[1-9][0-9]{2,}(-[0-9A-Za-z.-]+)?$`)

func init() {
	RegisterRuntime(&RuntimeKind{
		Name: "dotnet", Example: "8.0.404", Version: dotnetVersionRE,
		Provision: provisionDotnet,
		Bins:      []string{"."}, // the dotnet muxer sits in the install's root
		// DOTNET_ROOT as setup-dotnet exports it; the rest is what GitHub's
		// runner image sets in /etc/environment for every job.
		Vars: func(root string) []string {
			return []string{"DOTNET_ROOT=" + root, "DOTNET_NOLOGO=1", "DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1", "DOTNET_MULTILEVEL_LOOKUP=0"}
		},
		Caches: map[string]string{"nuget": ".nuget/packages"},
		Probe:  []string{"dotnet", "--version"}, // the SDK dotnet picks here, global.json included
	})
}

// provisionDotnet installs the .NET SDK <ver> for osName/arch from Microsoft's
// archive, checked against the SHA-512 the channel's releases.json lists for
// it. The archive is relocatable: dotnet finds everything relative to itself.
func provisionDotnet(dataDir, ver, osName, arch string) (string, error) {
	if !dotnetVersionRE.MatchString(ver) {
		return "", fmt.Errorf("not an SDK version: %q", ver)
	}
	rid, ok := map[string]string{"linux/amd64": "linux-x64", "linux/arm64": "linux-arm64", "darwin/amd64": "osx-x64", "darwin/arm64": "osx-arm64"}[osName+"/"+arch]
	if !ok {
		return "", fmt.Errorf("no .NET SDK build for %s/%s", osName, arch)
	}
	root := filepath.Join(dataDir, "runtimes", "dotnet")
	dest := filepath.Join(root, fmt.Sprintf("dotnet-sdk-%s-%s-%s", ver, osName, arch))
	if fi, err := os.Stat(filepath.Join(dest, "dotnet")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now) // last use, for the cleanup sweep
		return dest, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	channel := strings.Join(strings.SplitN(ver, ".", 3)[:2], ".")
	meta, err := httpGet(client, dotnetDist+"/release-metadata/"+channel+"/releases.json", 64<<20)
	if err != nil {
		return "", err
	}
	type sdk struct {
		Version string `json:"version"`
		Files   []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
			Hash string `json:"hash"`
		} `json:"files"`
	}
	var rels struct {
		Releases []struct {
			SDK  sdk   `json:"sdk"`
			SDKs []sdk `json:"sdks"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(meta, &rels); err != nil {
		return "", fmt.Errorf(".NET %s releases.json: %w", channel, err)
	}
	file := "dotnet-sdk-" + ver + "-" + rid + ".tar.gz"
	want := ""
	for _, r := range rels.Releases {
		for _, s := range append([]sdk{r.SDK}, r.SDKs...) {
			for _, f := range s.Files {
				if s.Version == ver && f.Name == "dotnet-sdk-"+rid+".tar.gz" && path.Base(f.URL) == file {
					want = f.Hash
				}
			}
		}
	}
	if len(want) != 128 {
		return "", fmt.Errorf("%s not in .NET %s's releases.json", file, channel)
	}
	tarball, err := httpGet(client, dotnetDist+"/Sdk/"+ver+"/"+file, 512<<20)
	if err != nil {
		return "", err
	}
	if h := sha512.Sum512(tarball); !strings.EqualFold(hex.EncodeToString(h[:]), want) {
		return "", errors.New("checksum mismatch")
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(tarball, tmp, "./"); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(tmp, "dotnet")); err != nil {
		return "", fmt.Errorf("%s has no dotnet executable at its root", file)
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, "dotnet")); serr == nil {
			return dest, nil // another process installed it first
		}
		return "", err
	}
	return dest, nil
}
