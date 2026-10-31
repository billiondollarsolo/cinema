package analyze

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// UbuntuLatest is the release GitHub's ubuntu-latest runs (seen 2026-10-31).
const UbuntuLatest = "ubuntu-26.04"

// toolCacheFolders: where setup-go, setup-node, setup-python and setup-java
// (Temurin) keep a runtime in the runner's tool cache:
// $RUNNER_TOOL_CACHE/<folder>/<version>/<arch>, complete when <arch>.complete
// exists (@actions/tool-cache's find; setup-java's findInToolcache). GitHub's
// images preinstall some versions there; without check-latest the action
// takes the newest one matching the spec before downloading anything.
var toolCacheFolders = map[string]string{"go": "go", "node": "node", "python": "Python", "java": "Java_Temurin-Hotspot_jdk"}

// UsesRunnerFirst reports whether runtime's setup action can use what the
// runner has preinstalled instead of the pinned release: the tool cache (Go,
// Node, Python, Java without check-latest) or, for .NET, the SDKs next to
// which setup-dotnet installs. Such floating: entries say which in
// preinstalled:.
func UsesRunnerFirst(runtime string) bool {
	return toolCacheFolders[runtime] != "" || runtime == "dotnet"
}

// ToolCache lists the complete versions of runtime in the tool cache at dir
// for this machine's arch, in pin form (temurin-21.0.8+9), prereleases left
// out as the actions leave them out of a range. An entry completed after this
// machine booted was installed by an earlier step of this job, not by the
// runner image, so it isn't what a fresh runner has: left out. No folder: an
// empty list (the action would download).
func ToolCache(dir, rt string) ([]string, error) {
	folder := toolCacheFolders[rt]
	if folder == "" {
		return nil, nil
	}
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[runtime.GOARCH]
	ents, err := os.ReadDir(filepath.Join(dir, folder))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	boot := bootTime()
	var out []string
	for _, e := range ents {
		name := e.Name()
		if fi, err := os.Stat(filepath.Join(dir, folder, name, arch)); err != nil || !fi.IsDir() {
			continue
		}
		done, err := os.Stat(filepath.Join(dir, folder, name, arch+".complete"))
		if err != nil || (!boot.IsZero() && done.ModTime().After(boot)) {
			continue
		}
		v := cachedPin(rt, name)
		if v != "" && rt == "java" {
			// The directory drops a respin's 4th number (11.0.32.1+1 is
			// 11.0.32-1); the JDK's release file has it.
			b, _ := os.ReadFile(filepath.Join(dir, folder, name, arch, "release"))
			v = cmp.Or(temurinRuntime(string(b)), v)
		}
		if v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// temurinRuntime is a JDK release file's JAVA_RUNTIME_VERSION as a Temurin
// pin, "" if absent. Adoptium's API folds a 4th version number into the
// build: 11.0.32.1+1 is 11.0.32+101 there (its two 11.0.32 builds: +9, +101).
func temurinRuntime(release string) string {
	for _, l := range strings.Split(release, "\n") {
		v, ok := strings.CutPrefix(strings.TrimSpace(l), "JAVA_RUNTIME_VERSION=")
		if !ok {
			continue
		}
		ver, build, _ := strings.Cut(strings.Trim(v, `"`), "+")
		build, _, _ = strings.Cut(build, "-") // 21.0.13+9-LTS
		n, err := strconv.Atoi(build)
		if err != nil {
			return ""
		}
		if f := strings.Split(ver, "."); len(f) == 4 {
			d, err := strconv.Atoi(f[3])
			if err != nil {
				return ""
			}
			ver, n = strings.Join(f[:3], "."), d*100+n
		}
		return task.TemurinVersion(ver + "+" + strconv.Itoa(n))
	}
	return ""
}

var semverDir = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// cachedPin is a tool cache directory name as a pin, "" for a prerelease or
// a name the action wouldn't take as a version. setup-java stores 21.0.8+9 as
// 21.0.8-9 (a "+" in JAVA_HOME broke Kotlin) and reads the first "-" back as
// "+"; its EA builds carry -ea.
func cachedPin(rt, name string) string {
	if !semverDir.MatchString(name) {
		return ""
	}
	if rt == "java" {
		if strings.Contains(name, "-ea") {
			return ""
		}
		return "temurin-" + strings.Replace(name, "-", "+", 1)
	}
	if strings.ContainsAny(name, "-+") {
		return "" // semver.satisfies leaves prereleases out of a range
	}
	return name
}

// bootTime is when this machine booted (Linux), zero elsewhere. A variable
// for tests.
var bootTime = func() time.Time {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for _, l := range strings.Split(string(b), "\n") {
		if s, ok := strings.CutPrefix(l, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				return time.Unix(n, 0)
			}
		}
	}
	return time.Time{}
}

// OnRunner is the version rt's setup action picks for a floating spec on a
// runner whose tool cache holds cached, given newest, the newest release
// matching spec (Resolve): with checkLatest, newest; else the newest cached
// version satisfying spec, or newest when none does (the action downloads
// it). setup-go's stable and setup-node's latest/current/node are resolved to
// the newest release before the cache is looked at; setup-node's lts/* to the
// newest LTS's major, which the cache then answers.
func OnRunner(rt, spec, newest string, checkLatest bool, cached []string) string {
	if checkLatest || BeforeCache(spec) {
		return newest
	}
	spec = strings.TrimSpace(strings.TrimPrefix(spec, "v"))
	if spec == "lts/*" || spec == "lts" {
		spec, _, _ = strings.Cut(newest, ".")
	}
	best := ""
	p := strings.TrimSuffix(strings.TrimSuffix(spec, ".x"), ".*")
	for _, v := range cached {
		if (v == p || strings.HasPrefix(v, p+".") || strings.HasPrefix(v, p+"+")) && (best == "" || slices.Compare(versionKey(v), versionKey(best)) > 0) {
			best = v
		}
	}
	return cmp.Or(best, newest)
}

// BeforeCache: the action resolves spec to the newest release before it looks
// in the tool cache (setup-go's stable, setup-node's latest/current/node), so
// the cache can't change what it uses.
func BeforeCache(spec string) bool {
	switch strings.TrimSpace(spec) {
	case "stable", "latest", "current", "node":
		return true
	}
	return false
}

// versionKey: the numbers of a pin (temurin-21.0.8+9: 21 0 8 9), for ordering.
func versionKey(v string) []int {
	v = strings.TrimPrefix(v, "temurin-")
	var k []int
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '+' || r == '-' }) {
		n, err := strconv.Atoi(f)
		if err != nil {
			break // a prerelease tag
		}
		k = append(k, n)
	}
	return k
}

// runnerImageReadme is actions/runner-images' software report for an Ubuntu
// release ("2604"): the image GitHub released last, with its tool cache.
var runnerImageReadme = "https://raw.githubusercontent.com/actions/runner-images/main/images/ubuntu/Ubuntu%s-Readme.md"

// imageCached is what the runner image for runsOn has in its tool cache for
// rt, in pin form, and the image's version, from the image's software report.
// That's the newest image GitHub released: during its rollout (days) some
// runners still have the previous one.
func imageCached(runsOn, rt string) (vers []string, image string, err error) {
	label := strings.ToLower(runsOn)
	if label == "" || label == "ubuntu-latest" {
		label = UbuntuLatest
	}
	rel, ok := strings.CutPrefix(label, "ubuntu-")
	if !ok || !regexp.MustCompile(`^[0-9]{2}\.04$`).MatchString(rel) {
		return nil, "", fmt.Errorf("no runner image report for %s", runsOn)
	}
	rs, err := list("image:" + strings.ReplaceAll(rel, ".", ""))
	if err != nil {
		return nil, "", err
	}
	for _, r := range rs {
		k, v, _ := strings.Cut(r.Version, " ")
		switch {
		case k == "image":
			image = v
		case k == rt && rt == "java":
			if v = temurinBuild(v); v != "" {
				vers = append(vers, v)
			}
		case k == rt:
			vers = append(vers, v)
		}
	}
	return vers, image, nil
}

// temurinBuild is the Adoptium build an image's Java version names: as
// written, or a respin's (the report's 11.0.32+1 is 11.0.32.1+1, Adoptium's
// 11.0.32+101; see temurinRuntime). "" if neither is published: the draft
// pins the newest release, and CI, which reads the JDK itself, decides.
func temurinBuild(v string) string {
	rs, err := list("java")
	if err != nil {
		return ""
	}
	ver, build, _ := strings.Cut(v, "+")
	n, _ := strconv.Atoi(build)
	for _, want := range []string{v, fmt.Sprintf("%s+%d", ver, 100+n)} {
		if slices.ContainsFunc(rs, func(r Release) bool { return r.Version == want }) {
			return want
		}
	}
	return ""
}

// imageReport parses a runner image's software report into releases named
// "<runtime> <pin>" (and "image <ImageVersion>"): Cached Tools' Go, Node.js
// and Python lists, and the Java table (Temurin, the image's tool cache too).
func imageReport(rel string) ([]Release, error) {
	b, err := fetch(fmt.Sprintf(runnerImageReadme, rel))
	if err != nil {
		return nil, err
	}
	var out []Release
	add := func(rt, v string) { out = append(out, Release{Version: rt + " " + v, Stable: true}) }
	section := ""
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "- Image Version: "):
			add("image", strings.TrimPrefix(l, "- Image Version: "))
		case strings.HasPrefix(l, "#"):
			section = map[string]string{"#### Go": "go", "#### Node.js": "node", "#### Python": "python", "### Java": "java"}[l]
		case section == "":
		case strings.HasPrefix(l, "- "):
			if v := strings.TrimPrefix(l, "- "); semverDir.MatchString(v) && section != "java" {
				add(section, v)
			}
		case section == "java" && strings.HasPrefix(l, "|"):
			f := strings.Fields(strings.Trim(l, "| "))
			if len(f) > 0 && semverDir.MatchString(f[0]) {
				add("java", "temurin-"+f[0])
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("runner image report for %s: nothing read", rel)
	}
	return out, nil
}

// DotnetSDKs is where the runner's .NET SDKs are (setup-dotnet's default
// install directory on Linux, next to the image's preinstalled ones).
var DotnetSDKs = "/usr/share/dotnet/sdk"

// NewerSDKs lists the SDKs in dir newer than pin: with any, dotnet on that
// runner doesn't use the pin (it takes the newest SDK unless global.json
// picks one), previews included.
func NewerSDKs(dir, pin string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && versionLess(pin, e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
