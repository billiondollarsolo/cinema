package analyze

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	exactBunDeno = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	bunToolLine  = regexp.MustCompile(`(?m)^bun\s*(.*?)$`)    // setup-bun's .tool-versions reader, as written
	denoToolLine = regexp.MustCompile(`(?m)^deno\s+v?(\S+)$`) // setup-deno's
	gitHash      = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)    // a Deno canary build
	tagVersion   = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)
)

func init() {
	RegisterSetupAction("oven-sh/setup-bun", setupBun)
	RegisterSetupAction("denoland/setup-deno", setupDeno)
	RegisterReleases("bun", bunReleases)
	RegisterReleases("deno", denoReleases)
}

// setupBun translates oven-sh/setup-bun (v2). Its version is bun-version, else
// bun-version-file, else package.json (packageManager bun@x, engines.bun),
// else the newest tag; a file it can't read falls through to the next.
func setupBun(c *SetupCtx) {
	if c.With("bun-download-url") != "" {
		c.Review("uses setup-bun's bun-download-url: a build cinema can't check against Bun's published checksums; pin a release in runtime:")
		return
	}
	if c.With("registries") != "" || c.With("registry-url") != "" {
		// They write bunfig.toml into the checkout, usually with a token.
		c.Review("uses setup-bun's registries: it writes bunfig.toml (registry and auth) before the job runs; reproduce it with commands")
	}
	// no-cache and token only change how setup-bun fetches bun itself.
	spec, from := strings.TrimSpace(withEnv(c.With("bun-version"), c.Env)), ""
	if spec == "" {
		if f := c.With("bun-version-file"); f != "" {
			spec, from = bunVersionFrom(c.Root, f), f
		}
		if spec == "" {
			if spec, from = bunVersionFrom(c.Root, "package.json"), "package.json"; spec == "" && c.Root == "" {
				c.Review("bun version comes from the repo's files, which cinema can't read here: pin it in runtime:")
				return
			}
		}
		if spec == "" {
			spec, from = "latest", ""
		}
	}
	if spec == "canary" || gitHash.MatchString(spec) {
		c.Review(fmt.Sprintf("bun-version %q is a canary build, not a release: pin a release in runtime:", spec))
		return
	}
	if v := pinFloating(c, "bun", spec, from); v != "" {
		// setup-bun puts ~/.bun/bin on PATH, where `bun add -g` puts commands.
		c.Step(`mkdir -p "$HOME/.bun/bin" && echo "$HOME/.bun/bin" >> "$GITHUB_PATH"`, "")
	}
}

// bunVersionFrom reads file as setup-bun does ("" if it has no version).
func bunVersionFrom(root, file string) string {
	if root == "" || !filepath.IsLocal(filepath.FromSlash(file)) {
		return ""
	}
	b := versionFromFile(filepath.Join(root, filepath.FromSlash(file)))
	switch filepath.Base(file) {
	case "package.json":
		var pkg struct {
			PackageManager string `json:"packageManager"`
			Engines        struct {
				Bun string `json:"bun"`
			} `json:"engines"`
		}
		if json.Unmarshal([]byte(b), &pkg) != nil {
			return ""
		}
		if _, v, ok := strings.Cut(pkg.PackageManager, "bun@"); ok {
			b = v
		} else {
			b = pkg.Engines.Bun
		}
	case ".tool-versions":
		m := bunToolLine.FindStringSubmatch(b)
		if m == nil {
			return ""
		}
		b = m[1]
	case ".bun-version", ".bumrc":
	default:
		return "" // setup-bun has no reader for it
	}
	return strings.TrimSpace(b)
}

// setupDeno translates denoland/setup-deno (v2): deno-version-file wins over
// deno-version, which defaults to 2.x.
func setupDeno(c *SetupCtx) {
	if n := c.With("deno-binary-name"); n != "" && n != "deno" {
		c.Review("uses setup-deno's deno-binary-name " + n + ": cinema installs deno as deno; reproduce the name with commands")
		return
	}
	// cache/cache-hash save DENO_DIR in Actions' cache: a download cache, which
	// cinema keeps per repo anyway (~/.cache/deno).
	spec, from := cmp.Or(strings.TrimSpace(withEnv(c.With("deno-version"), c.Env)), "2.x"), ""
	if f := c.With("deno-version-file"); f != "" {
		from = f
		if spec = denoVersionFrom(c.Root, f); spec == "" {
			c.Review("deno version comes from " + f + ", which cinema can't read (setup-deno fails without it): pin it in runtime:")
			return
		}
	}
	switch {
	case spec == "canary" || gitHash.MatchString(spec):
		c.Review(fmt.Sprintf("deno-version %q is a canary build, not a release: pin a release in runtime:", spec))
		return
	case spec == "rc" || spec == "lts":
		c.Review(fmt.Sprintf("deno-version %q: pin the exact release it names today in runtime:", spec))
		return
	}
	if v := pinFloating(c, "deno", spec, from); v != "" {
		// setup-deno puts DENO_INSTALL_ROOT (default ~/.deno/bin) on PATH, for `deno install -g`.
		c.Step(`echo "${DENO_INSTALL_ROOT:-$HOME/.deno/bin}" >> "$GITHUB_PATH"`, "")
	}
}

// denoVersionFrom reads file as setup-deno does: a .tool-versions deno line,
// else the whole file (.dvmrc).
func denoVersionFrom(root, file string) string {
	if root == "" || !filepath.IsLocal(filepath.FromSlash(file)) {
		return ""
	}
	b := versionFromFile(filepath.Join(root, filepath.FromSlash(file)))
	if m := denoToolLine.FindStringSubmatch(b); m != nil {
		return m[1]
	}
	return strings.TrimSpace(b)
}

// pinFloating pins rt from spec (exact, or resolved like Go and Node specs).
func pinFloating(c *SetupCtx, rt, spec, from string) string {
	if s := strings.TrimPrefix(spec, "v"); exactBunDeno.MatchString(s) {
		spec = s // both actions take v1.2.3 as 1.2.3
	}
	v := c.Exact(rt, spec, from, exactBunDeno)
	if v == "" {
		return ""
	}
	c.Pin(rt, v) // a resolved spec is recorded as floating: and checked again in CI
	return v
}

// bunRefs is the tag list of oven-sh/bun, the list setup-bun resolves ranges
// against (it asks GitHub's REST API, 60 unauthenticated requests an hour; a
// gate on shared runner IPs would run out). Git's own protocol serves the same
// refs without that limit: ls-refs, protocol v2, tags only (~20 KB).
var bunRefs = func() ([]byte, error) {
	body := pktLine("command=ls-refs\n") + "0001" + pktLine("ref-prefix refs/tags/bun-v\n") + "0000"
	req, err := http.NewRequest("POST", "https://github.com/oven-sh/bun.git/git-upload-pack", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", "version=2")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oven-sh/bun tags: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// bunReleases: every bun-v tag, newest first. All count as stable: setup-bun's
// "latest" is the highest tag, prerelease or not (the exact-version check then
// sends a prerelease pin to review).
func bunReleases() ([]Release, error) {
	b, err := bunRefs()
	if err != nil {
		return nil, err
	}
	var vs []string
	for len(b) >= 4 {
		n, err := strconv.ParseUint(string(b[:4]), 16, 16)
		if err != nil || (n > 0 && n < 4) || int(n) > len(b) {
			return nil, fmt.Errorf("oven-sh/bun tags: bad git response")
		}
		if n == 0 { // flush: the end
			break
		}
		f := strings.Fields(string(b[4:n]))
		b = b[n:]
		if len(f) >= 2 {
			if v, ok := strings.CutPrefix(f[1], "refs/tags/bun-v"); ok && tagVersion.MatchString(v) {
				vs = append(vs, v)
			}
		}
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("oven-sh/bun tags: none found")
	}
	return newestFirst(vs, func(string) bool { return true }), nil
}

// denoReleases: dl.deno.land/versions.json, the list setup-deno resolves
// ranges against (no rate limit); release candidates aren't stable.
func denoReleases() ([]Release, error) {
	b, err := fetch("https://dl.deno.land/versions.json")
	if err != nil {
		return nil, err
	}
	var v struct {
		CLI []string `json:"cli"`
	}
	if err := json.Unmarshal(b, &v); err != nil || len(v.CLI) == 0 {
		return nil, fmt.Errorf("dl.deno.land/versions.json: %v", cmp.Or[any](err, "no versions"))
	}
	var vs []string
	for _, s := range v.CLI {
		if s = strings.TrimPrefix(s, "v"); tagVersion.MatchString(s) {
			vs = append(vs, s)
		}
	}
	return newestFirst(vs, func(s string) bool { return !strings.Contains(s, "-") }), nil
}

// newestFirst sorts semver versions descending (a prerelease before its release).
func newestFirst(vs []string, stable func(string) bool) []Release {
	key := func(s string) (n [3]int, pre string) {
		core, pre, _ := strings.Cut(s, "-")
		for i, p := range strings.SplitN(core, ".", 3) {
			n[i], _ = strconv.Atoi(p)
		}
		return n, pre
	}
	slices.SortFunc(vs, func(a, b string) int {
		na, pa := key(a)
		nb, pb := key(b)
		if c := slices.Compare(nb[:], na[:]); c != 0 {
			return c
		}
		switch {
		case pa == pb:
			return 0
		case pa == "":
			return -1
		case pb == "":
			return 1
		}
		return strings.Compare(pb, pa) // prereleases only ever pin through review
	})
	vs = slices.Compact(vs)
	out := make([]Release, len(vs))
	for i, v := range vs {
		out[i] = Release{Version: v, Stable: stable(v)}
	}
	return out
}
