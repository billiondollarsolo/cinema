package analyze

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// actions/setup-java (Temurin), Gradle's setup actions, and Temurin releases
// for floating java-version specs.
func init() {
	RegisterSetupAction("actions/setup-java", setupJava)
	RegisterReleases("java", temurinReleases)
	RegisterReleases("gradle", gradleReleases)
	RegisterSetupAction("gradle/actions/setup-gradle", setupGradle)
	RegisterSetupAction("gradle/gradle-build-action", setupGradle)
	for _, a := range []string{"gradle/actions/wrapper-validation", "gradle/wrapper-validation-action"} {
		RegisterSetupAction(a, func(c *SetupCtx) { c.Step(gradleWrapperCheck, "") })
	}
	// GitHub's images default JAVA_HOME to their Temurin 11 (22.04), 17 (24.04)
	// or 25 (26.04), from Adoptium's apt repo, whose build moves with each
	// weekly image: no pin is that build reliably, and the gate couldn't tell.
	RegisterWithoutSetup("java", usesJava, func(c *SetupCtx) {
		c.Review("Java isn't set up by the job: it uses the runner image's default JDK (Temurin 11 on ubuntu-22.04, 17 on 24.04, 25 on 26.04), whose build changes with each image release; add actions/setup-java with an exact Temurin build")
	}, "actions/setup-java")
}

var usesJava = regexp.MustCompile(`(^|[\s;&|(])(\./)?(java|javac|mvnw?|gradlew?|sbt)\b`)

var (
	javaBuild = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\+[0-9]+$`) // 21.0.5+11: exactly that build
	javaLine  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(\.[x*])?$`)     // 21, 21.0, 21.x: the newest build of it
	javaPatch = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)         // 21.0.5: the newest build of 21.0.5
)

func setupJava(c *SetupCtx) {
	with := func(k string) string { return strings.TrimSpace(withEnv(c.With(k), c.Env)) }
	for _, k := range []string{"distribution", "java-version", "java-version-file", "java-package", "architecture", "check-latest", "set-default"} {
		if v := with(k); strings.Contains(v, "${{") {
			c.Review("setup-java's " + k + " uses an expression cinema can't evaluate: " + firstExpr(v))
			return
		}
	}
	// As setup-java: java-version wins over java-version-file; the file's
	// distribution (asdf, SDKMAN) wins over the distribution input.
	dist, spec := with("distribution"), ""
	var lines []string
	for _, l := range strings.Split(with("java-version"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	switch f := with("java-version-file"); {
	case len(lines) > 1:
		c.Review("setup-java installs several JDKs (java-version: " + strings.Join(lines, ", ") + "): cinema provisions one per task")
		return
	case len(lines) == 1:
		spec = lines[0]
	case f != "":
		v, d := javaVersionFile(c.Root, f)
		if v == "" {
			c.Review("Java version comes from " + f + ": cinema couldn't read one from it; pin an exact version in runtime:")
			return
		}
		spec, dist = v, cmp.Or(d, dist)
	default:
		c.Review("setup-java without java-version or java-version-file: CI's step fails")
		return
	}
	switch {
	case dist == "":
		c.Review("setup-java without a distribution: CI's step fails")
		return
	case dist != "temurin":
		// Another vendor's JDK of the same version is another build: never a stand-in.
		c.Review("setup-java distribution " + dist + ": cinema provisions Temurin only so far")
		return
	case with("java-package") != "" && with("java-package") != "jdk":
		c.Review("setup-java java-package " + with("java-package") + ": cinema provisions the JDK only")
		return
	case with("architecture") != "":
		c.Review("setup-java architecture " + with("architecture") + ": cinema provisions the JDK for the task's own platform; drop it or check it's the runner's")
		return
	case strings.EqualFold(with("set-default"), "false"):
		c.Review("setup-java set-default: false leaves JAVA_HOME and PATH alone; cinema always sets them")
		return
	}
	floating := false
	switch {
	case javaBuild.MatchString(spec):
	case javaLine.MatchString(spec):
		floating = true
	case javaPatch.MatchString(spec):
		// setup-java takes every build of it; Resolve's prefix match can't
		// (temurin-21.0.5 isn't a prefix of temurin-21.0.5+11 at a dot).
		c.Review(fmt.Sprintf("Java %q is the newest build of %s (Adoptium rebuilds publish as %s+101): cinema can't re-resolve that yet; use a build (%s+N) or a feature line (%s)", spec, spec, spec, spec, strings.Split(spec, ".")[0]))
		return
	default:
		c.Review(fmt.Sprintf("Java version %q: cinema provisions an exact Temurin build (21.0.5+11) or the newest of a feature line (21, 21.0); EA, latest and ranges stay with CI", spec))
		return
	}
	got := c.Exact("java", "temurin-"+spec, "", task.LookupRuntime("java").Version)
	if got == "" {
		return
	}
	c.Pin("java", got)
	if floating {
		if !strings.EqualFold(with("check-latest"), "true") {
			// Without check-latest, setup-java takes a matching JDK from the
			// runner's tool cache (hosted images carry Temurin 8/11/17/21/25),
			// which lags Adoptium's newest by up to an image release.
			c.Review(fmt.Sprintf("Java %q without check-latest: true: CI uses the runner image's preinstalled Temurin %s, not necessarily %s; set check-latest: true (or an exact build) so CI and the pin agree", spec, spec, got))
		}
	}
	javaMavenInputs(c, with)
	if !strings.EqualFold(with("show-download-progress"), "true") {
		// setup-java appends -ntp to MAVEN_ARGS (Maven 3.9+ reads it): logs only, but cheap to match.
		c.Step(`case " $MAVEN_ARGS " in *" -ntp "*|*" --no-transfer-progress "*) ;; *) echo "MAVEN_ARGS=${MAVEN_ARGS:+$MAVEN_ARGS }-ntp" >> "$GITHUB_ENV" ;; esac`, "")
	}
}

// javaMavenInputs: setup-java writes ~/.m2/settings.xml (servers whose
// credentials come from env, for deploys; GPG for signing) and toolchains.xml.
// cinema writes neither: a build that needs them fails locally, never passes.
func javaMavenInputs(c *SetupCtx, with func(string) string) {
	for _, k := range []string{"mvn-repositories", "mvn-plugin-repositories", "mvn-server-repository-origins", "settings-path"} {
		if with(k) != "" {
			c.Review("setup-java " + k + " configures Maven's ~/.m2/settings.xml (where dependencies come from); cinema doesn't write it")
		}
	}
	if with("gpg-private-key") != "" {
		c.Review("setup-java imports a GPG signing key (gpg-private-key): that's for publishing, and the key is a secret cinema doesn't have")
	}
	for _, k := range []string{"server-id", "server-username", "server-username-env-var", "server-password", "server-password-env-var", "mvn-server-credentials"} {
		if with(k) != "" {
			c.Note("setup-java's Maven server credentials (" + k + ") aren't written locally; they're for deploys, and a build that needs them fails rather than passes")
			break
		}
	}
	if with("mvn-toolchain-id") != "" || with("mvn-toolchain-vendor") != "" {
		c.Note("setup-java's ~/.m2/toolchains.xml isn't written locally: a build using maven-toolchains-plugin fails rather than passes")
	}
}

var (
	toolVersionsJava = regexp.MustCompile(`(?im)^java\s+(?:(\S*)-)?(\d+(?:\.\d+)*(?:[+_.-](?:openj9[-._]?\d[\w.-]*|java\d+|jre[-_\w]*|OpenJDK\d+[\w_.-]*|[a-z0-9]+))*)`)
	sdkmanJava       = regexp.MustCompile(`(?m)^java\s*=\s*([^-\s]+)(?:-([a-z0-9]+))?`)
	plainJava        = regexp.MustCompile(`(?:^|[\s-])(\d+\S*)(?:\s|$)`)
	// setup-java's vendor names in version files (mapAsdfDistribution, mapSdkmanDistribution).
	asdfVendors   = map[string]string{"temurin": "temurin", "adoptopenjdk": "temurin", "zulu": "zulu", "corretto": "corretto", "liberica": "liberica", "microsoft": "microsoft", "semeru": "semeru", "ibm": "semeru", "dragonwell": "dragonwell", "graalvm": "graalvm", "oracle": "oracle", "sapmachine": "sapmachine", "kona": "kona", "jetbrains": "jetbrains"}
	sdkmanVendors = map[string]string{"tem": "temurin", "sem": "semeru", "albba": "dragonwell", "dragonwell": "dragonwell", "zulu": "zulu", "amzn": "corretto", "graal": "graalvm", "graalce": "graalvm", "librca": "liberica", "ms": "microsoft", "oracle": "oracle", "sapmchn": "sapmachine", "jbr": "jetbrains", "kona": "kona"}
)

// javaVersionFile reads java-version-file as setup-java does: .tool-versions
// (asdf) and .sdkmanrc may name the vendor too; any other file holds the
// version. Unknown vendors fall back to the input, as there. "" if unreadable.
func javaVersionFile(root, file string) (version, dist string) {
	if root == "" || !filepath.IsLocal(filepath.FromSlash(file)) {
		return "", ""
	}
	b := readSmall(root, file)
	content := strings.TrimSpace(string(b))
	switch filepath.Base(file) {
	case ".tool-versions":
		if m := toolVersionsJava.FindStringSubmatch(content); m != nil {
			version, dist = m[2], strings.ToLower(m[1])
			switch {
			case strings.HasPrefix(dist, "graalvm-community"):
				dist = "graalvm-community"
			case strings.HasPrefix(dist, "oracle-graalvm"):
				dist = "graalvm"
			case dist != "":
				dist = asdfVendors[strings.Split(dist, "-")[0]]
			}
		}
	case ".sdkmanrc":
		if m := sdkmanJava.FindStringSubmatch(content); m != nil {
			version, dist = m[1], sdkmanVendors[strings.ToLower(m[2])]
		}
	default:
		if m := plainJava.FindStringSubmatch(content); m != nil {
			version = m[1]
		}
	}
	return strings.TrimPrefix(version, "1."), dist // 1.8 is 8
}

// temurinReleases lists Temurin GA JDKs, newest first, for linux x64 and
// aarch64 together (the lister doesn't know the job's arch). The union is
// safe: a pin that is the newest of both is the newest of the job's arch,
// or has no build for it and so no evidence.
func temurinReleases() ([]Release, error) {
	seen := map[string]bool{}
	var vers []string
	for _, arch := range []string{"x64", "aarch64"} {
		for page := 0; page < 20; page++ {
			b, err := fetch(fmt.Sprintf("https://api.adoptium.net/v3/info/release_versions?architecture=%s&os=linux&image_type=jdk&jvm_impl=hotspot&heap_size=normal&release_type=ga&vendor=eclipse&project=jdk&sort_order=DESC&page_size=50&page=%d", arch, page))
			if err != nil && page > 0 && strings.Contains(err.Error(), "404") {
				break // past the last page
			}
			if err != nil {
				return nil, err
			}
			var v struct {
				Versions []struct {
					Semver string `json:"semver"`
				} `json:"versions"`
			}
			if err := json.Unmarshal(b, &v); err != nil {
				return nil, fmt.Errorf("Adoptium release list: %w", err)
			}
			for _, r := range v.Versions {
				if tv := task.TemurinVersion(r.Semver); tv != "" && !seen[tv] {
					seen[tv] = true
					vers = append(vers, tv)
				}
			}
			if len(v.Versions) < 50 {
				break
			}
		}
	}
	// Newest first as setup-java orders them (semver, then build).
	slices.SortFunc(vers, func(a, b string) int { return slices.Compare(javaKey(b), javaKey(a)) })
	out := make([]Release, len(vers))
	for i, v := range vers {
		out[i] = Release{Version: v, Stable: true}
	}
	return out, nil
}

func javaKey(v string) []int {
	var k []int
	for _, f := range strings.FieldsFunc(strings.TrimPrefix(v, "temurin-"), func(r rune) bool { return r == '.' || r == '+' }) {
		n, _ := strconv.Atoi(f)
		k = append(k, n)
	}
	return k
}

// gradleReleases lists Gradle releases for gradle-version: current, the
// release services.gradle.org flags current (as setup-gradle resolves it), first.
func gradleReleases() ([]Release, error) {
	b, err := fetch("https://services.gradle.org/versions/all")
	if err != nil {
		return nil, err
	}
	var all []struct {
		Version, RcFor, MilestoneFor                       string
		Current, Snapshot, Nightly, ReleaseNightly, Broken bool
	}
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("Gradle release list: %w", err)
	}
	var out []Release
	for _, r := range all {
		if r.Snapshot || r.Nightly || r.ReleaseNightly || r.Broken || r.RcFor != "" || r.MilestoneFor != "" ||
			!task.LookupRuntime("gradle").Version.MatchString(r.Version) {
			continue
		}
		rel := Release{Version: r.Version, Stable: true}
		if r.Current {
			out = slices.Insert(out, 0, rel)
		} else {
			out = append(out, rel)
		}
	}
	return out, nil
}

// gradleWrapperCheck is setup-gradle's validate-wrappers (on by default since
// v4): every gradle-wrapper.jar must be a published one. Stricter here: the
// jar must be the one of the release its gradle-wrapper.properties names. A
// jar from another release fails locally and the job runs in CI.
const gradleWrapperCheck = `find . -name .git -prune -o -name gradle-wrapper.jar -type f -print | while IFS= read -r jar; do
  v=$(sed -nE 's/^distributionUrl=.*gradle-(.+)-(bin|all)\.zip$/\1/p' "${jar%.jar}.properties" 2>/dev/null)
  want=$(curl -fsSL "https://services.gradle.org/distributions/gradle-$v-wrapper.jar.sha256" || true)
  got=$(sha256sum "$jar" 2>/dev/null || shasum -a 256 "$jar")
  [ -n "$v" ] && [ "${got%% *}" = "$want" ] || { echo "$jar isn't Gradle ${v:-?}'s wrapper jar (setup-gradle validates wrappers)" >&2; exit 1; }
done`

func setupGradle(c *SetupCtx) {
	with := func(k string) string { return strings.TrimSpace(withEnv(c.With(k), c.Env)) }
	for _, k := range []string{"gradle-version", "arguments", "build-root-directory", "validate-wrappers", "develocity-injection-enabled"} {
		if v := with(k); strings.Contains(v, "${{") {
			c.Review("the Gradle action's " + k + " uses an expression cinema can't evaluate: " + firstExpr(v))
			return
		}
	}
	gradle := "./gradlew"
	switch v := with("gradle-version"); {
	case v == "" || v == "wrapper":
	case v == "current" || task.LookupRuntime("gradle").Version.MatchString(v):
		got := c.Exact("gradle", v, "", task.LookupRuntime("gradle").Version)
		if got == "" {
			return
		}
		c.Pin("gradle", got)
		gradle = "gradle"
	default:
		c.Review("gradle-version " + v + ": cinema provisions an exact Gradle release or current; release candidates and nightlies stay with CI")
		return
	}
	if strings.EqualFold(with("develocity-injection-enabled"), "true") || with("develocity-access-key") != "" {
		// A Develocity server's remote build cache replays task outputs, tests included.
		c.Review("the Gradle action connects the build to Develocity (remote build cache, build scans): cinema runs without it")
		return
	}
	if dg := with("dependency-graph"); dg != "" && dg != "disabled" {
		c.Note("the Gradle action's dependency graph (" + dg + ") is a submission, not a check: not reproduced")
	}
	// gradle-build-action v2 ran the build itself.
	if a := with("arguments"); a != "" {
		if strings.ContainsAny(a, "$`;|&<>\\\n(){}*?'\"") {
			c.Review("the Gradle action's arguments need parsing cinema doesn't do: write them as a run step")
			return
		}
		c.Step(gradle+" "+a, with("build-root-directory"))
	}
	if !strings.EqualFold(with("validate-wrappers"), "false") {
		c.Step(gradleWrapperCheck, "") // runs first: Step inserts right after the action
	}
	c.Note("the Gradle action's caching doesn't change results: the build runs as the job's steps say")
}
