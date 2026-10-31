package analyze

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// jdx/mise-action and asdf-vm/actions install the tools a repo's mise.toml /
// .tool-versions name: translated into pins of the runtimes cinema provisions,
// read as mise and asdf read them.
func init() {
	RegisterSetupAction("jdx/mise-action", miseAction)
	RegisterSetupAction("asdf-vm/actions/install", asdfInstall)
	RegisterSetupAction("asdf-vm/actions/setup", func(c *SetupCtx) {
		c.Review("asdf-vm/actions/setup installs asdf only: which tools the job's own asdf commands install isn't known; use asdf-vm/actions/install, or pin the tools in runtime:")
	})
}

// miseRuntimes maps mise and asdf tool names to runtime: keys.
var miseRuntimes = map[string]string{"node": "node", "nodejs": "node", "go": "go", "golang": "go", "python": "python",
	"java": "java", "rust": "rust", "bun": "bun", "deno": "deno", "dotnet": "dotnet", "dotnet-core": "dotnet", "uv": "uv", "gradle": "gradle"}

// miseName is a tool's name as one key: nodejs and core:node are node.
func miseName(n string) string {
	n = strings.TrimPrefix(n, "core:")
	if rt, ok := miseRuntimes[n]; ok {
		return rt
	}
	return n
}

// miseFiles: the config files mise reads in a directory, lowest precedence
// first (mise.jdx.dev/configuration: dotfiles beat their plain names, local
// files beat shared ones).
var miseFiles = []string{".tool-versions", ".config/mise/config.toml", ".config/mise/mise.toml", ".config/mise.toml",
	".mise/config.toml", "mise/config.toml", "mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml"}

type miseTool struct {
	spec string            // as written ("22", "temurin-21"); several are space-separated
	opts map[string]string // tool options (rust's components)
	file string
}

func miseAction(c *SetupCtx) {
	for _, k := range []string{"install", "bootstrap", "install_args", "working_directory", "tool_versions", "mise_toml", "env", "cache", "plugins"} {
		if v := c.With(k); strings.Contains(v, "${{") {
			c.Review("mise-action's " + k + " uses an expression cinema can't evaluate: " + firstExpr(v))
			return
		}
	}
	switch {
	case c.With("install") == "false":
		c.Review("mise-action install: false: the tools come from a cache or later steps; pin them in runtime:")
		return
	case c.With("bootstrap") == "true":
		c.Review("mise-action bootstrap: true runs mise bootstrap (tools, tasks, hooks): translate it")
		return
	case c.With("plugins") != "":
		c.Review("mise-action installs plugins: cinema provisions its own runtimes, not plugins'")
		return
	case c.Env["MISE_ENV"] != "":
		c.Review("MISE_ENV selects mise." + c.Env["MISE_ENV"] + ".toml config files: pin the tools in runtime:")
		return
	}
	only := map[string]bool{} // install_args: only these tools are installed
	for _, a := range strings.Fields(c.With("install_args")) {
		switch name, _, _ := strings.Cut(a, "@"); {
		case a == "--locked":
		case strings.HasPrefix(a, "-"):
			c.Review("mise-action install_args " + a + ": pin the tools in runtime:")
			return
		default:
			only[miseName(name)] = true
		}
	}
	inputs := map[string]string{}
	if v := c.With("tool_versions"); v != "" {
		inputs[".tool-versions"] = v // the action writes them in the working directory
	}
	if v := c.With("mise_toml"); v != "" {
		inputs["mise.toml"] = v
	}
	tools, env, why := miseConfig(c.Root, c.With("working_directory"), inputs, miseFiles, true)
	if why != "" {
		c.Review("mise: " + why)
		return
	}
	for name := range tools {
		if len(only) > 0 && !only[name] {
			delete(tools, name) // not installed: its shim fails in CI too
		}
	}
	if misePins(c, "mise", tools) && c.With("cache") != "false" {
		// The cache key is the config's hash: a floating version stays at what
		// was installed first until cache_max_age_days (30) rebuilds it.
		c.Review("mise-action caches installed tools by the config's hash (cache: true) for up to 30 days: a floating version can stay at an older release in CI than today's pin; pin exact versions (or mise.lock), or set cache: false")
	}
	if c.With("env") != "false" && len(env) > 0 {
		var b strings.Builder
		for _, k := range sortedKeys(env) {
			b.WriteString("echo " + shellQuote(k+"="+env[k]) + ` >> "$GITHUB_ENV"` + "\n")
		}
		c.Step(strings.TrimSuffix(b.String(), "\n"), "")
		c.Note("mise-action exports mise.toml's [env] for later steps: mirrored with $GITHUB_ENV")
	}
}

// asdfInstall: asdf-vm/actions/install installs .tool-versions' tools
// (tool_versions: written over it first); asdf reads no other file.
func asdfInstall(c *SetupCtx) {
	if c.With("before_install") != "" {
		c.Review("asdf-vm/actions/install before_install runs commands before installing: translate them")
		return
	}
	inputs := map[string]string{}
	if v := c.With("tool_versions"); v != "" {
		inputs[".tool-versions"] = v
	}
	tools, _, why := miseConfig(c.Root, "", inputs, miseFiles[:1], false)
	if why != "" {
		c.Review("asdf: " + why)
		return
	}
	misePins(c, "asdf", tools)
}

// miseConfig merges the tools and [env] of files in the repo root and then in
// wd (deeper wins, as mise walks up from where it runs), a later file's tool
// entry replacing an earlier one's; inputs are file contents an action wrote
// in wd. With lock, mise.lock's versions win over the config's specs (the
// action adds --locked when there is one).
func miseConfig(root, wd string, inputs map[string]string, files []string, lock bool) (map[string]miseTool, map[string]string, string) {
	tools, env := map[string]miseTool{}, map[string]string{}
	if root == "" {
		return tools, env, ""
	}
	dirs := []string{"."}
	if wd != "" && filepath.Clean(wd) != "." {
		if !filepath.IsLocal(filepath.FromSlash(wd)) {
			return nil, nil, "working_directory " + wd + " is outside the repo"
		}
		if _, err := statRepo(root, wd); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, "working_directory " + wd + " leads outside the repo (a symlink?)"
		}
		dirs = append(dirs, filepath.ToSlash(filepath.Clean(wd)))
	}
	locked := map[string]string{}
	for i, d := range dirs {
		for _, cd := range []string{"mise/conf.d", ".mise/conf.d", ".config/mise/conf.d"} {
			// Anything there, or a path that leaves the repo: mise would read it.
			if _, err := statRepo(root, path.Join(d, cd)); !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, "config fragments in " + cd + ": pin the tools in runtime:"
			}
		}
		for _, f := range files {
			name := filepath.ToSlash(filepath.Join(d, f))
			src, given := inputs[f]
			if !given || i != len(dirs)-1 {
				b := readSmall(root, name)
				if b == nil {
					continue
				}
				src = string(b)
			}
			var why string
			if f == ".tool-versions" {
				why = parseToolVersions(src, name, tools)
			} else {
				why = parseMiseTOML(src, name, tools, env)
			}
			if why != "" {
				return nil, nil, why
			}
		}
		if b := readSmall(root, path.Join(d, "mise.lock")); lock && b != nil {
			if why := parseMiseLock(string(b), locked); why != "" {
				return nil, nil, why
			}
		}
	}
	for name, v := range locked {
		if t, ok := tools[name]; ok {
			t.spec, t.file = v, "mise.lock"
			tools[name] = t
		}
	}
	return tools, env, ""
}

// parseToolVersions reads asdf's format: `tool version [fallback…]` lines.
func parseToolVersions(src, file string, tools map[string]miseTool) string {
	for _, line := range strings.Split(src, "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) == 1 {
			return file + ": " + f[0] + " has no version"
		}
		tools[miseName(f[0])] = miseTool{spec: strings.Join(f[1:], " "), file: file}
	}
	return ""
}

var (
	miseToolSection = regexp.MustCompile(`^\[tools\.(?:"([^"]+)"|([A-Za-z0-9_:-]+))\]$`)
	miseLockSection = regexp.MustCompile(`^\[(\[)?tools\.(?:"([^"]+)"|([A-Za-z0-9_:-]+))\]\]?$`)
	miseInline      = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*("[^"]*"|'[^']*'|\[[^\]]*\]|[^,}\s]+)`)
	envName         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// parseMiseTOML reads what decides a mise.toml's tools and environment:
// [tools] (strings, arrays, inline tables, [tools.<name>] tables) and [env].
// Anything else that changes them (hooks, aliases, plugins, idiomatic version
// files, env directives, templates) is a why.
func parseMiseTOML(src, file string, tools map[string]miseTool, env map[string]string) string {
	lines := strings.Split(src, "\n")
	section, tool := "", ""
	bad := func(what string) string { return file + ": " + what + ": pin the tools in runtime:" }
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(rustUncomment(lines[i]))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			if m := miseToolSection.FindStringSubmatch(line); m != nil {
				tool, section = miseName(m[1]+m[2]), "[tool]"
				tools[tool] = miseTool{opts: map[string]string{}, file: file} // replaces the whole entry
			}
			for _, p := range []string{"[hooks", "[alias", "[tool_alias", "[plugins", "[[env", "[env."} {
				if strings.HasPrefix(section, p) {
					return bad(section)
				}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k, v = strings.Trim(strings.TrimSpace(k), `"'`), strings.TrimSpace(v)
		for ok && i+1 < len(lines) && (strings.HasPrefix(v, "[") && !strings.Contains(v, "]") || strings.HasPrefix(v, "{") && !strings.Contains(v, "}")) {
			i++
			v += " " + strings.TrimSpace(rustUncomment(lines[i]))
		}
		if !ok {
			return bad("unexpected " + strconv.Quote(line))
		}
		switch section {
		case "":
			if strings.HasPrefix(k, "tools.") || strings.HasPrefix(k, "env") || k == "dotenv" {
				return bad(k)
			}
		case "[settings]":
			if strings.Contains(k, "idiomatic_version_file") || strings.Contains(k, "legacy_version_file") {
				return bad("settings " + k + " (version files other tools read)")
			}
		case "[tools]":
			t := miseTool{opts: map[string]string{}, file: file}
			if strings.HasPrefix(v, "{") {
				for _, m := range miseInline.FindAllStringSubmatch(v, -1) {
					if m[1] == "version" {
						t.spec = miseValue(m[2])
					} else {
						t.opts[m[1]] = miseValue(m[2])
					}
				}
			} else if t.spec = strings.Join(tomlStrings(v), " "); t.spec == "" {
				return bad("tool " + k + " = " + v)
			}
			tools[miseName(k)] = t
		case "[tool]":
			t := tools[tool]
			if k == "version" {
				t.spec = strings.Join(tomlStrings(v), " ")
			} else {
				t.opts[k] = miseValue(v)
			}
			tools[tool] = t
		case "[env]":
			s := tomlStrings(v)
			switch {
			case k == "_" || strings.HasPrefix(k, "_."):
				return bad("[env] directive " + k + " (_.path, _.file, _.source, _.python.venv)")
			case !envName.MatchString(k) || k == "PATH":
				return bad("[env] " + k)
			case len(s) == 1 && strings.Contains(s[0], "{{"):
				return bad("[env] " + k + " is a template")
			case len(s) == 1 && v[0] != '[':
				env[k] = s[0]
			case regexp.MustCompile(`^-?[0-9]+$`).MatchString(v):
				env[k] = v
			default:
				return bad("[env] " + k + " = " + v)
			}
		}
	}
	return ""
}

// tomlStrings: the strings in a TOML value (one, or an array's).
func tomlStrings(v string) []string {
	var out []string
	for _, m := range rustTOMLString.FindAllStringSubmatch(v, -1) {
		out = append(out, m[1]+m[2])
	}
	return out
}

// miseValue: a string, an array as "a,b", or a bare value as written.
func miseValue(v string) string {
	if s := tomlStrings(v); len(s) > 0 {
		return strings.Join(s, ",")
	}
	return v
}

// parseMiseLock reads mise.lock's version of each tool ([tools.node] or
// [[tools.node]] then version = "…").
func parseMiseLock(src string, lock map[string]string) string {
	cur, seen := "", map[string]bool{}
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(rustUncomment(line))
		if strings.HasPrefix(line, "[") {
			cur = ""
			if m := miseLockSection.FindStringSubmatch(line); m != nil {
				if cur = miseName(m[2] + m[3]); seen[cur] {
					return "mise.lock has several versions of " + cur + ": cinema provisions one"
				}
				seen[cur] = true
			}
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && cur != "" && strings.TrimSpace(k) == "version" {
			if s := tomlStrings(v); len(s) == 1 {
				lock[cur] = s[0]
			}
		}
	}
	return ""
}

var javaLTSBuild = regexp.MustCompile(`(\+[0-9]+)(\.0)?\.LTS$`) // asdf-java and mise name Temurin builds 21.0.5+11.0.LTS

// misePins pins each tool cinema provisions; others go to review, named. It
// reports whether a pin came from a floating spec.
func misePins(c *SetupCtx, who string, tools map[string]miseTool) (floating bool) {
	var other []string
	for _, name := range sortedKeys(tools) {
		t := tools[name]
		rt, ok := miseRuntimes[name]
		if !ok {
			other = append(other, name+" "+t.spec)
			continue
		}
		bad := func(why string) { c.Review(who + ": " + name + " " + t.spec + " (" + t.file + "): " + why) }
		spec := strings.TrimPrefix(strings.TrimPrefix(t.spec, "prefix:"), "latest:") // mise's prefix:1.25, asdf's latest:22
		switch {
		case spec == "":
			bad("no version")
			continue
		case strings.Contains(spec, " "):
			bad("several versions: cinema provisions one per task")
			continue
		case spec == "system":
			bad("the runner's preinstalled one: pin it in runtime:")
			continue
		case strings.Contains(spec, "{{"), strings.HasPrefix(spec, "ref:"), strings.HasPrefix(spec, "path:"), strings.HasPrefix(spec, "sub-"):
			bad("not a release cinema can provision")
			continue
		}
		if k := otherOption(t.opts, rt); k != "" {
			bad("tool option " + k + ": pin it in runtime:")
			continue
		}
		switch rt {
		case "rust":
			// mise sets RUSTUP_TOOLCHAIN, which beats a rust-toolchain file.
			extras := append(rustList(t.opts["components"]), rustList(t.opts["targets"])...)
			switch t.opts["profile"] {
			case "", "minimal": // rustup's configured profile on the runner
			case "default":
				extras = append(extras, "clippy", "rustfmt")
			default:
				bad("rustup profile " + t.opts["profile"])
				continue
			}
			floating = floating || !rustExactRE.MatchString(spec)
			rustPin(c, spec, extras)
			continue
		case "java":
			vendor, v, _ := strings.Cut(javaLTSBuild.ReplaceAllString(spec, "$1"), "-")
			switch {
			case v == "":
				bad("mise's java without a vendor is OpenJDK's build; cinema provisions Temurin (temurin-21)")
				continue
			case vendor != "temurin":
				bad("cinema provisions Temurin only so far")
				continue
			case javaBuild.MatchString(v):
			case javaLine.MatchString(v):
				floating = true
			default:
				bad("cinema provisions an exact Temurin build (temurin-21.0.5+11) or the newest of a feature line (temurin-21)")
				continue
			}
			if got := c.Exact("java", "temurin-"+v, "", task.LookupRuntime("java").Version); got != "" {
				c.Pin("java", got)
			}
			continue
		case "node":
			if spec == "lts" {
				spec = "lts/*"
			}
		}
		exact := map[string]*regexp.Regexp{"go": regexp.MustCompile(`^1\.[0-9]+\.[0-9]+$`), "node": regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)}[rt]
		if exact == nil {
			exact = task.LookupRuntime(rt).Version
		}
		if got := c.Exact(rt, spec, "", exact); got != "" {
			floating = floating || !exact.MatchString(spec)
			c.Pin(rt, got)
		}
	}
	if len(other) > 0 {
		c.Review(who + " installs tools cinema doesn't provision (" + strings.Join(other, ", ") + "): keep the job in CI, or drop them from the task if its steps don't use them")
	}
	return floating
}

// otherOption is a tool option mise applies that cinema doesn't reproduce
// ("" if none): rustup's components, targets and profile are part of the pin.
func otherOption(opts map[string]string, rt string) string {
	for _, k := range sortedKeys(opts) {
		if rt != "rust" || (k != "components" && k != "targets" && k != "profile") {
			return k
		}
	}
	return ""
}
