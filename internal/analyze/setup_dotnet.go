package analyze

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// dotnetIndex lists .NET's channels with each one's newest SDK: what
// setup-dotnet's "8.0.x" (the 8.0 channel) installs.
const dotnetIndex = "https://builds.dotnet.microsoft.com/dotnet/release-metadata/releases-index.json"

func init() {
	RegisterSetupAction("actions/setup-dotnet", setupDotnet)
	RegisterReleases("dotnet", dotnetReleases)
	// The runner's 11 preinstalled SDKs: dotnet picks the newest, or global.json's
	// roll-forward among them, so which one depends on the image's set.
	RegisterWithoutSetup("dotnet", regexp.MustCompile(`(^|[\s;&|(])dotnet\b`), func(c *SetupCtx) {
		c.Review(".NET isn't set up by the job: it uses one of the runner image's preinstalled SDKs (the newest, or global.json's roll-forward among them), which change with each image release; add actions/setup-dotnet with an exact SDK")
	}, "actions/setup-dotnet")
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
			out = append(out, Release{Version: c.LatestSDK, Stable: c.Phase != "preview", GoLive: c.Phase == "go-live"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return versionLess(out[j].Version, out[i].Version) })
	return out, nil
}

// newestGASDK is the newest SDK of the newest channel that's past release
// candidates (go-live is an RC), "" if unknown.
func newestGASDK() string {
	rs, err := list("dotnet")
	if err != nil {
		return ""
	}
	for _, r := range rs {
		if r.Stable && !r.GoLive {
			return r.Version
		}
	}
	return ""
}

// olderGlobalJSON is a global.json in the repo that can make dotnet pick an
// SDK older than the newest installed, "" if none (dotnet reads the nearest
// one up from where it runs). No sdk.version, or a latest* rollForward
// (latestPatch is the default), takes the newest SDK that satisfies it: the
// pin when CI has none newer, else nothing locally (only the pin is there),
// so the task fails rather than passes. patch, feature, minor and major take
// the lowest that does: an image's 10.0.1xx over the pinned 10.0.403.
func olderGlobalJSON(root string) string {
	found := ""
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		switch {
		case err != nil:
			return nil
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules"):
			return filepath.SkipDir
		case d.IsDir() || d.Name() != "global.json":
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		b, err := readRepo(root, filepath.ToSlash(rel))
		var g struct {
			SDK struct {
				Version     string `json:"version"`
				RollForward string `json:"rollForward"`
			} `json:"sdk"`
		}
		if err == nil {
			err = json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))), &g)
		}
		if err != nil || g.SDK.Version != "" && slices.Contains([]string{"patch", "feature", "minor", "major"}, g.SDK.RollForward) {
			found = filepath.ToSlash(rel)
			return filepath.SkipAll
		}
		return nil
	})
	return found
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
	shared := dir == "" || strings.HasPrefix(dir, "/usr/")
	if !shared && c.With("skip-lts-prepass") != "true" {
		c.Review("setup-dotnet first installs the newest LTS .NET runtime and its dotnet host (it floats, and apps may roll forward to it): set skip-lts-prepass: true so CI uses exactly the pinned SDK")
	}

	var pins []string
	floated := false
	for _, s := range specs {
		v := ""
		switch {
		case exact.MatchString(s):
			v = s
		case dotnetBand.MatchString(s), s == "*", strings.EqualFold(s, "x"), strings.EqualFold(s, "lts"), strings.EqualFold(s, "sts"):
			c.Review(fmt.Sprintf(".NET version %q floats in a way cinema can't check again in CI yet: pin an exact SDK in runtime:", s))
		default:
			v = c.Exact("dotnet", s, "", exact) // recorded as floating: when resolved
			floated = v != ""
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
	if shared {
		// Next to the image's SDKs, dotnet uses the newest SDK and apps the
		// newest runtime patch present. With one floating spec on the newest
		// GA channel, that's the pin's own: the image's SDKs are that channel's
		// or older, and CI runs the job if its runner has any newer (lab:
		// NSubstitute's 10.0.x used 10.0.401, that day's newest). An older
		// channel loses to the image's newer SDKs; global.json picks among them.
		why := ""
		switch ga := newestGASDK(); {
		case !floated || len(specs) != 1:
			why = "only a single floating spec on the newest GA channel (10.0.x) is the SDK dotnet uses there"
		case ga != pins[0]:
			why = fmt.Sprintf("%s isn't the newest GA SDK (%s), so the image's newer SDKs win", pins[0], cmp.Or(ga, "unknown"))
		case c.Root == "":
			why = "the repo isn't here to look for global.json"
		default:
			if f := olderGlobalJSON(c.Root); f != "" {
				why = f + " can select an older SDK among the image's (or isn't plain JSON)"
			}
		}
		c.Preinstalled("dotnet") // also for a review: approved as is, CI still checks
		if why != "" {
			c.Review("setup-dotnet installs into /usr/share/dotnet next to the runner's preinstalled SDKs and runtimes: dotnet there picks the newest SDK unless global.json selects one, and apps run on the newest runtime patch present; " + why + ". Give it a directory of its own (job env DOTNET_INSTALL_DIR: ~/.dotnet-sdk, and skip-lts-prepass: true) so CI uses exactly the pinned SDK")
		} else {
			c.Note("setup-dotnet installs " + pins[0] + " next to the runner's preinstalled SDKs: it's the newest GA SDK, so dotnet uses it there; CI checks its runner has no newer SDK on every run (preinstalled:)")
		}
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
	b, err := readRepo(root, file) // the workflow names it: untrusted
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
