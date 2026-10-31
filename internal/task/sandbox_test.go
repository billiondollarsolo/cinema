package task

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A task can rewrite its checkout's git alternates; the next sandbox must not
// mount whatever host path it names (security review).
func TestSandboxRefusesForeignAlternates(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".git", "objects", "info", "alternates"), []byte("/home/me/.ssh\n"), 0o644)
	_, err := StartSandbox(context.Background(), SandboxSpec{Image: "x@sha256:" + strings.Repeat("0", 64), Platform: "linux/amd64",
		Root: root, Objects: "/real/repo/.git/objects", DataDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "refusing to mount") {
		t.Fatalf("got %v", err)
	}
}

// Runtime caches are one volume per checkout slot: Gradle, cargo, NuGet and
// Maven lock their caches in ways another container can't see (Gradle timed
// out waiting for a build in the next container).
func TestRuntimeCacheMountsPerSlot(t *testing.T) {
	caches := map[string]string{"gradle": ".gradle/caches/modules-2", "m2": ".m2/repository/cached"}
	args, dirs, err := runtimeCacheMounts("cinema-cache-abc", "arm64", "/data/checkouts/abc/3", caches)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-v", "cinema-cache-abc-gradle-arm64-3:/home/runner/.gradle/caches/modules-2",
		"-v", "cinema-cache-abc-m2-arm64-3:/home/runner/.m2/repository/cached"}
	if strings.Join(args, " ") != strings.Join(want, " ") || len(dirs) != 2 || dirs[1] != "/home/runner/.m2/repository/cached" {
		t.Fatalf("got %q %q", args, dirs)
	}
	other, _, _ := runtimeCacheMounts("cinema-cache-abc", "arm64", "/data/checkouts/abc/4", caches)
	if other[1] == args[1] {
		t.Fatal("two slots share a volume")
	}
	if _, _, err := runtimeCacheMounts("cinema-cache-abc", "arm64", "/home/me/src/repo", caches); err == nil {
		t.Fatal("a working tree isn't a slot: sharing its caches with every other container must fail")
	}
	if args, _, err := runtimeCacheMounts("cinema-cache-abc", "arm64", "/home/me/src/repo", nil); err != nil || args != nil {
		t.Fatalf("no caches: got %q %v", args, err)
	}
}

// Maven's local repository: downloads in the cache volume, the project's own
// installed artifacts in the container (the next run must not resolve them).
func TestMavenInstallsStayInTheContainer(t *testing.T) {
	if p := LookupRuntime("java").Caches["m2"]; p != ".m2/repository/cached" {
		t.Fatalf("m2 cache at %s: installs would land in the shared volume", p)
	}
	if !strings.Contains(mavenRC, "-Daether.enhancedLocalRepository.split=true") || !strings.Contains(mavenRC, "$MAVEN_OPTS ") {
		t.Fatalf("mavenRC %s must split the repository and keep the job's MAVEN_OPTS", mavenRC)
	}
}
