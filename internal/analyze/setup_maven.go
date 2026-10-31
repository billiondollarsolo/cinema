package analyze

import (
	"regexp"
	"strings"

	"github.com/billiondollarsolo/cinema/internal/task"
)

// JVM build tools called without a wrapper (mvn, gradle, ant, sbt): what
// GitHub's Ubuntu images have (actions/runner-images, toolset-*.json and
// install-java-tools.sh), and Maven's setup action.
func init() {
	RegisterSetupAction("stCarolas/setup-maven", setupMaven)
	// The images link /usr/bin/mvn to the Apache zip: on 22.04 and 24.04 the
	// newest 3.x when the image was built (toolset "3": 3.10.0 in images of
	// 2026-10-26), on 26.04 the toolset's 3.9.15. cinema's runner image installs
	// the same zip the same way (runner/Dockerfile), so the draft pins nothing
	// and probes mvn on both sides, as for the runner's python3: a pin would be
	// provisioned on the CI runner too (verify's RuntimeEnv) and probe cinema's
	// Maven there, never the one the job runs. When GitHub's moves, the job
	// runs in CI until the runner image follows.
	RegisterWithoutSetup("maven", usesMvn, func(c *SetupCtx) {
		c.Probe("mvn", "--version")
		c.Note("Maven isn't set up by the job: it uses the runner image's mvn (3.10.0 on ubuntu-22.04 and 24.04, 3.9.15 on 26.04), which cinema's runner image has too; mvn --version is fingerprinted")
	}, "stCarolas/setup-maven", "s4u/setup-maven-action")
	// /usr/bin/gradle is the newest Gradle when the image was built (9.8.2 in
	// images of 2026-10-26): it moves with Gradle's monthly releases, and a
	// 150 MB distribution in the runner image would lag it more often than not.
	RegisterWithoutSetup("gradle", usesGradle, func(c *SetupCtx) {
		c.Review("Gradle isn't set up by the job: `gradle` is the runner image's, the newest release when the image was built (9.8.2 in October 2026), which moves with each image; use ./gradlew or setup-gradle with an exact gradle-version")
	})
	RegisterWithoutSetup("ant", usesAnt, func(c *SetupCtx) {
		c.Review("Ant: GitHub's images have Ubuntu's apt ant (1.10.12 on 22.04, 1.10.14 on 24.04, 1.10.15 on 26.04); cinema's runner image doesn't (apt's ant brings a JDK of its own), so this stays in CI")
	})
	RegisterWithoutSetup("sbt", usesSbt, func(c *SetupCtx) {
		c.Review("sbt: only GitHub's ubuntu-22.04 image has the sbt launcher (1.13.0; sbt/setup-sbt installs it elsewhere), and cinema provisions no sbt, so this stays in CI")
	})
}

// A bare command, not ./mvnw or a path like gradle/libs.versions.toml.
var (
	usesMvn    = bareCommand("mvn")
	usesGradle = bareCommand("gradle")
	usesAnt    = bareCommand("ant")
	usesSbt    = bareCommand("sbt")
)

func bareCommand(name string) *regexp.Regexp {
	return regexp.MustCompile(`(^|[\s;&|(])` + name + `([\s;&|)]|$)`)
}

// setupMaven: stCarolas/setup-maven puts Maven maven-version (exact; default
// 3.8.2) from Maven Central on PATH.
func setupMaven(c *SetupCtx) {
	v := strings.TrimSpace(withEnv(c.With("maven-version"), c.Env))
	if !c.Has("maven-version") {
		v = "3.8.2"
	}
	switch {
	case strings.Contains(v, "${{"):
		c.Review("setup-maven's maven-version uses an expression cinema can't evaluate: " + firstExpr(v))
	case !task.LookupRuntime("maven").Version.MatchString(v):
		c.Review("setup-maven maven-version " + v + ": cinema provisions an exact Maven release")
	default:
		c.Pin("maven", v)
	}
}
