package task

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Java: Eclipse Temurin JDKs (setup-java's distribution: temurin) from the
// Adoptium API, and Gradle distributions (setup-gradle's gradle-version).
//
// A java version names its vendor: temurin-21.0.5+11 is Adoptium's semver
// for the build (a rebuild like 17.0.20.1+1 is 17.0.20+101), without the
// .0.LTS that LTS builds append (21.0.5+11.0.LTS) and setup-java ignores when
// matching 21.0.5+11. Another vendor's 21.0.5 is another JDK, so receipts
// never cross vendors.
func init() {
	RegisterRuntime(&RuntimeKind{
		Name: "java", Example: "temurin-21.0.5+11", Version: javaVersionRE,
		Provision: provisionJava,
		Bins:      []string{"bin"},
		Vars:      javaVars,
		// Dependency downloads only. Not all of ~/.gradle/caches: its
		// build-cache-1 holds task outputs (test results included) that a
		// build with org.gradle.caching would replay instead of running.
		Caches: map[string]string{"m2": ".m2/repository", "gradle": ".gradle/caches/modules-2", "gradle-wrapper": ".gradle/wrapper/dists"},
		Probe:  []string{"java", "-version"}, // prints to stderr: probes capture both
	})
	// No probe: gradle --version prints the kernel release, which differs
	// between any two machines. The exact pin is in the contract.
	RegisterRuntime(&RuntimeKind{Name: "gradle", Example: "8.14.3", Version: gradleVersionRE, Provision: provisionGradle, Bins: []string{"bin"}})
}

var (
	javaVersionRE   = regexp.MustCompile(`^temurin-([0-9]+)\.[0-9]+\.[0-9]+\+[0-9]+$`)
	javaInstallRE   = regexp.MustCompile(`^temurin-([0-9]+)\..*-(linux|darwin)-(amd64|arm64)$`)
	gradleVersionRE = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)
)

// TemurinVersion is the java version of an Adoptium semver
// ("21.0.5+11.0.LTS" → "temurin-21.0.5+11"), or "" if it isn't a GA build's.
func TemurinVersion(semver string) string {
	v, build, _ := strings.Cut(semver, "+")
	build, _, _ = strings.Cut(build, ".")
	if s := "temurin-" + v + "+" + build; javaVersionRE.MatchString(s) {
		return s
	}
	return ""
}

// Adoptium's API, and where its binaries live (variables so tests use a fake).
var (
	adoptiumAPI  = "https://api.adoptium.net"
	temurinLinks = "https://github.com/adoptium/"
)

// javaVars: what setup-java exports. JAVA_HOME_<major>_<ARCH> is ARM64 from
// setup-java v4 (os.arch()) and AARCH64 from releases that normalize the arch:
// both, so a job written for either finds it.
func javaVars(root string) []string {
	vars := []string{"JAVA_HOME=" + root}
	name := filepath.Base(strings.TrimSuffix(root, filepath.Join("Contents", "Home")))
	if m := javaInstallRE.FindStringSubmatch(name); m != nil {
		for _, a := range map[string][]string{"amd64": {"X64"}, "arm64": {"ARM64", "AARCH64"}}[m[3]] {
			vars = append(vars, "JAVA_HOME_"+m[1]+"_"+a+"="+root)
		}
	}
	return vars
}

// provisionJava installs Temurin ver for osName/arch under
// <data>/runtimes/java/<ver>-<os>-<arch> and returns its JAVA_HOME (the
// tarball's root; Contents/Home on macOS).
func provisionJava(dataDir, ver, osName, arch string) (string, error) {
	m := javaVersionRE.FindStringSubmatch(ver)
	apiOS := map[string]string{"linux": "linux", "darwin": "mac"}[osName]
	apiArch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[arch]
	switch {
	case m == nil:
		return "", fmt.Errorf("not a Temurin version: %q", ver)
	case apiOS == "" || apiArch == "":
		return "", fmt.Errorf("no Temurin build for %s/%s", osName, arch)
	}
	home := func(dir string) string {
		if osName == "darwin" {
			return filepath.Join(dir, "Contents", "Home")
		}
		return dir
	}
	root := filepath.Join(dataDir, "runtimes", "java")
	dest := filepath.Join(root, ver+"-"+osName+"-"+arch)
	if fi, err := os.Stat(filepath.Join(home(dest), "bin", "java")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now) // last use, for the cleanup sweep
		return home(dest), nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	pkg, dir, err := temurinPackage(client, m[1], ver, apiOS, apiArch)
	if err != nil {
		return "", err
	}
	if len(pkg.Checksum) != 64 || !strings.HasPrefix(pkg.Link, temurinLinks) || !strings.HasSuffix(pkg.Link, ".tar.gz") {
		return "", fmt.Errorf("Adoptium lists %s for %s, not an Adoptium tarball with a SHA-256", pkg.Link, ver)
	}
	tarball, err := httpGet(client, pkg.Link, 512<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(tarball); hex.EncodeToString(h[:]) != strings.ToLower(pkg.Checksum) {
		return "", errors.New("checksum mismatch")
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(tarball, tmp, dir+"/"); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(home(tmp), "bin", "java")); err != nil {
		return "", fmt.Errorf("%s: no bin/java under %s/", pkg.Link, dir)
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(home(dest), "bin", "java")); serr == nil {
			return home(dest), nil // another process installed it first
		}
		return "", err
	}
	return home(dest), nil
}

type temurinPkg struct {
	Name     string `json:"name"`
	Link     string `json:"link"`
	Checksum string `json:"checksum"`
}

// temurinPackage finds the JDK tarball of java version ver in Adoptium's GA
// releases of major, newest first, 20 a page as the API allows. dir is the
// tarball's top directory (the release name, jdk-21.0.5+11).
func temurinPackage(c *http.Client, major, ver, apiOS, apiArch string) (temurinPkg, string, error) {
	q := url.Values{"architecture": {apiArch}, "os": {apiOS}, "image_type": {"jdk"}, "jvm_impl": {"hotspot"},
		"heap_size": {"normal"}, "vendor": {"eclipse"}, "project": {"jdk"}, "sort_order": {"DESC"}, "page_size": {"20"}}
	for page := 0; page < 50; page++ {
		q.Set("page", fmt.Sprint(page))
		b, found, err := adoptiumGet(c, adoptiumAPI+"/v3/assets/feature_releases/"+major+"/ga?"+q.Encode())
		if err != nil {
			return temurinPkg{}, "", err
		}
		if !found {
			break
		}
		var rels []struct {
			ReleaseName string `json:"release_name"`
			VersionData struct {
				Semver string `json:"semver"`
			} `json:"version_data"`
			Binaries []struct {
				OS           string     `json:"os"`
				Architecture string     `json:"architecture"`
				ImageType    string     `json:"image_type"`
				Package      temurinPkg `json:"package"`
			} `json:"binaries"`
		}
		if err := json.Unmarshal(b, &rels); err != nil {
			return temurinPkg{}, "", fmt.Errorf("Adoptium API: %w", err)
		}
		if len(rels) == 0 {
			break
		}
		for _, r := range rels {
			if TemurinVersion(r.VersionData.Semver) != ver || strings.ContainsAny(r.ReleaseName, "/\\") || r.ReleaseName == "" {
				continue
			}
			for _, bin := range r.Binaries {
				if bin.OS == apiOS && bin.Architecture == apiArch && bin.ImageType == "jdk" {
					return bin.Package, r.ReleaseName, nil
				}
			}
		}
	}
	return temurinPkg{}, "", fmt.Errorf("no %s JDK for %s/%s in Adoptium's releases", ver, apiOS, apiArch)
}

// adoptiumGet is httpGet, except that a page past the last one (404) is
// found=false rather than an error.
func adoptiumGet(c *http.Client, u string) (b []byte, found bool, err error) {
	resp, err := c.Get(u)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	b, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return b, err == nil, err
}

// gradleDist is Gradle's distribution host (a variable so tests use a fake).
var gradleDist = "https://services.gradle.org/distributions"

// provisionGradle installs the official Gradle ver (bin zip, checked against
// the SHA-256 Gradle publishes beside it) under <data>/runtimes/gradle. It's
// JVM bytecode and scripts: one install for every OS and arch.
func provisionGradle(dataDir, ver, osName, arch string) (string, error) {
	if !gradleVersionRE.MatchString(ver) {
		return "", fmt.Errorf("not a Gradle release: %q", ver)
	}
	if osName != "linux" && osName != "darwin" {
		return "", fmt.Errorf("no Gradle provisioning for %s", osName)
	}
	name := "gradle-" + ver
	root := filepath.Join(dataDir, "runtimes", "gradle")
	dest := filepath.Join(root, name)
	if fi, err := os.Stat(filepath.Join(dest, "bin", "gradle")); err == nil && fi.Mode().IsRegular() {
		now := time.Now()
		_ = os.Chtimes(dest, now, now)
		return dest, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	sum, err := httpGet(client, gradleDist+"/"+name+"-bin.zip.sha256", 1<<10)
	if err != nil {
		return "", err
	}
	want := strings.ToLower(strings.TrimSpace(string(sum)))
	if len(want) != 64 {
		return "", fmt.Errorf("%s-bin.zip.sha256 isn't a SHA-256", name)
	}
	archive, err := httpGet(client, gradleDist+"/"+name+"-bin.zip", 300<<20)
	if err != nil {
		return "", err
	}
	if h := sha256.Sum256(archive); hex.EncodeToString(h[:]) != want {
		return "", errors.New("checksum mismatch")
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extractZip(archive, tmp, name+"/"); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, serr := os.Stat(filepath.Join(dest, "bin", "gradle")); serr == nil {
			return dest, nil
		}
		return "", err
	}
	return dest, nil
}

// extractZip unpacks entries under prefix into dir: files and directories
// only, none escaping dir.
func extractZip(data []byte, dir, prefix string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		rel, ok := strings.CutPrefix(f.Name, prefix)
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return fmt.Errorf("zip entry %q escapes the install dir", f.Name)
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("zip entry %q isn't a file or directory", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		perm := mode.Perm()&0o755 | 0o644 // readable even from a zip made without Unix modes
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		in, err := f.Open()
		if err == nil {
			_, err = io.Copy(out, in)
			in.Close()
		}
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
