package analyze

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// dotnetIndex lists .NET's channels with each one's newest SDK: what
// setup-dotnet's "8.0.x" (the 8.0 channel) installs.
const dotnetIndex = "https://builds.dotnet.microsoft.com/dotnet/release-metadata/releases-index.json"

func init() {
	RegisterSetupAction("actions/setup-dotnet", setupDotnet)
	RegisterReleases("dotnet", dotnetReleases)
}

// dotnetReleases: each channel's newest SDK, newest channel first. A channel
// in preview isn't Stable (setup-dotnet's "latest" skips it); a go-live one
// (release candidates) is, as setup-dotnet treats it.
func dotnetReleases() ([]Release, error) {
	b, err := fetch(dotnetIndex)
	if err != nil {
		return nil, err
	}
	var idx struct {
		Channels []struct {
			LatestSDK string `json:"latest-sdk"`
			Phase     string `json:"support-phase"`
		} `json:"releases-index"`
	}
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf(".NET releases index: %w", err)
	}
	var out []Release
	for _, c := range idx.Channels {
		if c.LatestSDK != "" {
			out = append(out, Release{Version: c.LatestSDK, Stable: c.Phase != "preview"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return versionLess(out[j].Version, out[i].Version) })
	return out, nil
}

// versionLess compares the numeric parts of two versions (prereleases aside).
func versionLess(a, b string) bool {
	pa, pb := strings.Split(strings.SplitN(a, "-", 2)[0], "."), strings.Split(strings.SplitN(b, "-", 2)[0], ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

var (
	dotnetBand = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]xx$`)
	sdkFull    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[1-9][0-9]{2,}$`) // setup-dotnet's versionPattern
)

// setupDotnet translates actions/setup-dotnet. On GitHub's runners it installs
// into /usr/share/dotnet, next to the SDKs the image preinstalls (8.0 to 10.0,
// several bands each) and, first, the newest LTS runtime: there dotnet uses
// the newest SDK unless global.json picks one, and apps roll forward to the
// newest runtime patch present, so neither is the one SDK a pin provisions.
// Only an install of its own (DOTNET_INSTALL_DIR, skip-lts-prepass) is the pin.
func setupDotnet(c *SetupCtx) {
	exact := task.LookupRuntime("dotnet").Version
	var specs []string
	for _, l := range strings.Split(c.With("dotnet-version"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			specs = append(specs, l)
		}
	}
	gj := c.With("global-json-file")
	if gj == "" && len(specs) == 0 && c.Root != "" {
		if _, err := os.Lstat(filepath.Join(c.Root, "global.json")); err == nil {
			gj = "global.json" // the action's fallback: the repo root's
		}
	}
	if gj != "" {
		v, why := globalJSONSDK(c.Root, gj)
		if why != "" {
			c.Review("setup-dotnet reads " + gj + ": " + why + "; pin the SDK in runtime:")
			return
		}
		specs = append(specs, v)
	}
	if len(specs) == 0 {
		c.Review("setup-dotnet installs no SDK here (no dotnet-version, no global.json): the job uses the runner's preinstalled .NET; pin one in runtime:")
		return
	}
	if ch := c.With("dotnet-channel"); ch != "" {
		if i := slices.IndexFunc(specs, func(s string) bool { return strings.EqualFold(s, "latest") }); i >= 0 {
			specs[i] = ch // latest in channel 8.0 is the 8.0 channel's newest
		}
	}
	for _, k := range []string{"workloads", "source-url"} {
		if c.With(k) != "" {
			c.Review(map[string]string{
				"workloads":  "setup-dotnet installs workloads (" + c.With(k) + "): they're written into the SDK, which cinema mounts read-only, and their manifests float",
				"source-url": "setup-dotnet configures a NuGet feed with credentials (source-url: " + c.With(k) + "): reproduce it without secrets, or keep the job in CI",
			}[k])
		}
	}
	if q := strings.ToLower(c.With("dotnet-quality")); q != "" && q != "ga" {
		c.Review("setup-dotnet installs a " + q + " build: it changes without a version change")
	}
	if a := strings.ToLower(c.With("architecture")); a != "" && a != "x64" && a != "amd64" {
		c.Review("setup-dotnet installs an " + a + " SDK on an x64 runner")
	}
	dir := c.Env["DOTNET_INSTALL_DIR"]
	switch {
	case dir == "" || strings.HasPrefix(dir, "/usr/"):
		c.Review("setup-dotnet installs into /usr/share/dotnet next to the runner's preinstalled SDKs and runtimes: dotnet there picks the newest SDK unless global.json selects one, and apps run on the newest runtime patch present. Give it a directory of its own (job env DOTNET_INSTALL_DIR: ~/.dotnet-sdk, and skip-lts-prepass: true) so CI uses exactly the pinned SDK")
	case c.With("skip-lts-prepass") != "true":
		c.Review("setup-dotnet first installs the newest LTS .NET runtime and its dotnet host (it floats, and apps may roll forward to it): set skip-lts-prepass: true so CI uses exactly the pinned SDK")
	}

	var pins []string
	for _, s := range specs {
		v := ""
		switch {
		case exact.MatchString(s):
			v = s
		case dotnetBand.MatchString(s), s == "*", strings.EqualFold(s, "x"), strings.EqualFold(s, "lts"), strings.EqualFold(s, "sts"):
			c.Review(fmt.Sprintf(".NET version %q floats in a way cinema can't check again in CI yet: pin an exact SDK in runtime:", s))
		default:
			v = c.Exact("dotnet", s, "", exact) // recorded as floating: when resolved
		}
		if v != "" && !slices.Contains(pins, v) {
			pins = append(pins, v)
		}
	}
	switch len(pins) {
	case 0:
		return
	case 1:
	default:
		c.Review("setup-dotnet installs several SDKs (" + strings.Join(pins, ", ") + "): a task pins one, and with several dotnet uses the newest unless global.json picks one")
	}
	c.Pin("dotnet", pins[0])
	// GitHub's image puts ~/.dotnet/tools (dotnet tool install -g) on PATH.
	c.Step(`echo "$HOME/.dotnet/tools" >> "$GITHUB_PATH"`, "")
}

// globalJSONSDK is the SDK spec setup-dotnet installs for a global.json: its
// sdk.version, widened by rollForward as the action widens it. why says what
// stops it.
func globalJSONSDK(root, file string) (spec, why string) {
	if root == "" || !filepath.IsLocal(filepath.FromSlash(file)) {
		return "", "it can't be read here"
	}
	// The workflow names the file: no symlinks, regular files only, capped.
	f, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(file)), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "it can't be read here"
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", "it can't be read here"
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return "", "it can't be read here"
	}
	var g struct {
		SDK struct {
			Version     string `json:"version"`
			RollForward string `json:"rollForward"`
		} `json:"sdk"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))), &g); err != nil {
		return "", "it isn't plain JSON (comments?)"
	}
	v, rf := g.SDK.Version, g.SDK.RollForward
	switch {
	case v == "":
		return "", "it names no SDK version"
	case rf == "" || strings.Contains(v, "-"):
		return v, ""
	case !sdkFull.MatchString(v):
		return "", "rollForward needs a full SDK version, and the action fails without one"
	}
	p := strings.SplitN(v, ".", 3)
	switch rf {
	case "latestMajor":
		return "", "rollForward: latestMajor uses the newest SDK on the runner"
	case "latestMinor":
		return p[0], ""
	case "latestFeature":
		return p[0] + "." + p[1], ""
	case "latestPatch":
		return p[0] + "." + p[1] + "." + p[2][:1] + "xx", ""
	}
	return v, ""
}
