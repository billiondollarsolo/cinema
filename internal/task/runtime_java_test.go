package task

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAdoptium serves Adoptium's feature_releases for major 21, in two pages
// (the wanted build on the second, as for an older release), and the tarball;
// sum overrides the listed checksum.
func fakeAdoptium(t *testing.T, apiOS, apiArch string, tarball []byte, sum string) {
	if sum == "" {
		h := sha256.Sum256(tarball)
		sum = hex.EncodeToString(h[:])
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rel := func(name, semver string) map[string]any {
			return map[string]any{"release_name": name, "version_data": map[string]any{"semver": semver},
				"binaries": []map[string]any{{"os": q.Get("os"), "architecture": q.Get("architecture"), "image_type": "jdk",
					"package": map[string]any{"name": name + ".tar.gz", "link": srv.URL + "/dl/" + name + ".tar.gz", "checksum": sum}}}}
		}
		switch {
		case r.URL.Path == "/v3/assets/feature_releases/21/ga" && q.Get("os") == apiOS && q.Get("architecture") == apiArch && q.Get("image_type") == "jdk" && q.Get("page") == "0":
			json.NewEncoder(w).Encode([]any{rel("jdk-21.0.6+7", "21.0.6+7")})
		case r.URL.Path == "/v3/assets/feature_releases/21/ga" && q.Get("page") == "1":
			json.NewEncoder(w).Encode([]any{rel("jdk-21.0.5+11", "21.0.5+11.0.LTS")})
		case r.URL.Path == "/dl/jdk-21.0.5+11.tar.gz":
			w.Write(tarball)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldAPI, oldLinks := adoptiumAPI, temurinLinks
	adoptiumAPI, temurinLinks = srv.URL, srv.URL+"/dl/"
	t.Cleanup(func() { adoptiumAPI, temurinLinks = oldAPI, oldLinks })
}

func TestJavaRuntimeProvisioned(t *testing.T) {
	k := LookupRuntime("java")
	if k == nil || !k.Version.MatchString("temurin-21.0.5+11") || !k.Version.MatchString("temurin-17.0.20+101") ||
		k.Version.MatchString("21.0.5+11") || k.Version.MatchString("zulu-21.0.5+11") || k.Version.MatchString("temurin-21") {
		t.Fatal("java version format")
	}
	for semver, want := range map[string]string{"21.0.5+11.0.LTS": "temurin-21.0.5+11", "17.0.20+101": "temurin-17.0.20+101", "27.0.0-beta+35": "", "21": ""} {
		if got := TemurinVersion(semver); got != want {
			t.Errorf("TemurinVersion(%s) = %q, want %q", semver, got, want)
		}
	}
	fakeAdoptium(t, "linux", "aarch64", tarGz(t, map[string]string{"jdk-21.0.5+11/bin/java": "#!/bin/sh\n", "jdk-21.0.5+11/release": "x"}), "")
	data := t.TempDir()
	root, err := k.Provision(data, "temurin-21.0.5+11", "linux", "arm64")
	want := filepath.Join(data, "runtimes", "java", "temurin-21.0.5+11-linux-arm64")
	if err != nil || root != want {
		t.Fatalf("%q %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "java")); err != nil {
		t.Fatal(err)
	}
	vars := strings.Join(k.Vars(root), " ")
	if vars != "JAVA_HOME="+root+" JAVA_HOME_21_ARM64="+root+" JAVA_HOME_21_AARCH64="+root {
		t.Fatalf("vars: %s", vars)
	}
	// Installed: found again without the network.
	adoptiumAPI = "http://127.0.0.1:1"
	if again, err := k.Provision(data, "temurin-21.0.5+11", "linux", "arm64"); err != nil || again != root {
		t.Fatalf("reuse: %q %v", again, err)
	}
	if _, err := k.Provision(data, "temurin-21.0.5+11", "windows", "amd64"); err == nil {
		t.Fatal("windows: want an error")
	}
}

// macOS tarballs put the JDK under Contents/Home: that's JAVA_HOME.
func TestJavaRuntimeMacLayout(t *testing.T) {
	fakeAdoptium(t, "mac", "x64", tarGz(t, map[string]string{"jdk-21.0.5+11/Contents/Home/bin/java": "x", "jdk-21.0.5+11/Contents/Info.plist": "x"}), "")
	data := t.TempDir()
	root, err := provisionJava(data, "temurin-21.0.5+11", "darwin", "amd64")
	if err != nil || root != filepath.Join(data, "runtimes", "java", "temurin-21.0.5+11-darwin-amd64", "Contents", "Home") {
		t.Fatalf("%q %v", root, err)
	}
	if v := javaVars(root); len(v) != 2 || v[1] != "JAVA_HOME_21_X64="+root {
		t.Fatalf("vars: %v", v)
	}
}

func TestJavaRuntimeChecksumMismatch(t *testing.T) {
	fakeAdoptium(t, "linux", "x64", tarGz(t, map[string]string{"jdk-21.0.5+11/bin/java": "x"}), strings.Repeat("0", 64))
	data := t.TempDir()
	if _, err := provisionJava(data, "temurin-21.0.5+11", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(data, "runtimes", "java")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
	// A build Adoptium doesn't list, and a link off Adoptium's host, are refused.
	if _, err := provisionJava(data, "temurin-21.0.4+7", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "no temurin-21.0.4+7 JDK") {
		t.Fatalf("unlisted build: %v", err)
	}
	temurinLinks = "https://github.com/adoptium/"
	if _, err := provisionJava(data, "temurin-21.0.5+11", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "not an Adoptium tarball") {
		t.Fatalf("foreign link: %v", err)
	}
}

func javaZipOf(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func fakeGradle(t *testing.T, ver string, archive []byte, sum string) {
	if sum == "" {
		h := sha256.Sum256(archive)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gradle-" + ver + "-bin.zip.sha256":
			fmt.Fprint(w, sum)
		case "/gradle-" + ver + "-bin.zip":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := gradleDist
	gradleDist = srv.URL
	t.Cleanup(func() { gradleDist = old })
}

func TestGradleRuntime(t *testing.T) {
	k := LookupRuntime("gradle")
	if k == nil || !k.Version.MatchString("8.14.3") || !k.Version.MatchString("9.0") || k.Version.MatchString("9.0-rc-1") {
		t.Fatal("gradle version format")
	}
	fakeGradle(t, "8.14.3", javaZipOf(t, map[string]string{"gradle-8.14.3/bin/gradle": "#!/bin/sh\n", "gradle-8.14.3/lib/x.jar": "x"}), "")
	data := t.TempDir()
	root, err := k.Provision(data, "8.14.3", "linux", "amd64")
	if err != nil || root != filepath.Join(data, "runtimes", "gradle", "gradle-8.14.3") {
		t.Fatalf("%q %v", root, err)
	}
	if fi, err := os.Stat(filepath.Join(root, "bin", "gradle")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("bin/gradle: %v %v", fi, err)
	}

	fakeGradle(t, "8.14.2", javaZipOf(t, map[string]string{"gradle-8.14.2/bin/gradle": "x"}), strings.Repeat("0", 64))
	if _, err := k.Provision(data, "8.14.2", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	fakeGradle(t, "8.14.1", javaZipOf(t, map[string]string{"gradle-8.14.1/../../evil": "x"}), "")
	if _, err := k.Provision(data, "8.14.1", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("want traversal refused, got %v", err)
	}
}

// fakeMaven serves Apache's archive layout for ver (bin zip and .sha512);
// sum overrides the published checksum.
func fakeMaven(t *testing.T, ver string, archive []byte, sum string) {
	if sum == "" {
		h := sha512.Sum512(archive)
		sum = hex.EncodeToString(h[:])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bin := "/maven-3/" + ver + "/binaries/apache-maven-" + ver + "-bin.zip"
		switch r.URL.Path {
		case bin + ".sha512":
			fmt.Fprint(w, sum+"  apache-maven-"+ver+"-bin.zip\n") // older releases name the file too
		case bin:
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := mavenDist
	mavenDist = srv.URL
	t.Cleanup(func() { mavenDist = old })
}

func TestMavenRuntime(t *testing.T) {
	k := LookupRuntime("maven")
	if k == nil || !k.Version.MatchString("3.9.15") || k.Version.MatchString("3.9") || k.Version.MatchString("4.0.0-rc-4") ||
		strings.Join(k.Probe, " ") != "mvn --version" {
		t.Fatal("maven version format or probe")
	}
	fakeMaven(t, "3.10.0", javaZipOf(t, map[string]string{"apache-maven-3.10.0/bin/mvn": "#!/bin/sh\n", "apache-maven-3.10.0/lib/x.jar": "x"}), "")
	data := t.TempDir()
	root, err := k.Provision(data, "3.10.0", "linux", "amd64")
	if err != nil || root != filepath.Join(data, "runtimes", "maven", "apache-maven-3.10.0") {
		t.Fatalf("%q %v", root, err)
	}
	if fi, err := os.Stat(filepath.Join(root, "bin", "mvn")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("bin/mvn: %v %v", fi, err)
	}
	mavenDist = "http://127.0.0.1:1" // installed: found again without the network
	if again, err := k.Provision(data, "3.10.0", "darwin", "arm64"); err != nil || again != root {
		t.Fatalf("reuse: %q %v", again, err)
	}

	fakeMaven(t, "3.9.15", javaZipOf(t, map[string]string{"apache-maven-3.9.15/bin/mvn": "x"}), strings.Repeat("0", 128))
	if _, err := k.Provision(data, "3.9.15", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum mismatch, got %v", err)
	}
	// A SHA-256 where Apache publishes SHA-512 isn't the checksum.
	fakeMaven(t, "3.9.14", javaZipOf(t, map[string]string{"apache-maven-3.9.14/bin/mvn": "x"}), strings.Repeat("0", 64))
	if _, err := k.Provision(data, "3.9.14", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "isn't a checksum") {
		t.Fatalf("want a short checksum refused, got %v", err)
	}
	fakeMaven(t, "3.9.13", javaZipOf(t, map[string]string{"apache-maven-3.9.13/lib/x.jar": "x"}), "")
	if _, err := k.Provision(data, "3.9.13", "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "no bin/mvn") {
		t.Fatalf("want a zip without bin/mvn refused, got %v", err)
	}
	if left, _ := os.ReadDir(filepath.Join(data, "runtimes", "maven")); len(left) != 1 {
		t.Fatalf("left behind: %v", left)
	}
}

// mvn --version prints the JVM, the kernel and (JDK 24+) native-access
// warnings: only the release line is fingerprinted, on both sides.
func TestProbeReleaseLine(t *testing.T) {
	gh := "WARNING: A restricted method in java.lang.System has been called\n\x1b[1mApache Maven 3.10.0 (abc123)\x1b[m\nMaven home: /usr/share/apache-maven-3.10.0\nJava version: 21.0.5, vendor: Eclipse Adoptium\nOS name: \"linux\", version: \"6.8.0-1017-azure\""
	local := "Apache Maven 3.10.0 (abc123)\r\nMaven home: /usr/share/apache-maven-3.10.0\r\nOS name: \"linux\", version: \"6.10.14-linuxkit\""
	if a, b := probeLine([]string{"mvn", "--version"}, gh), probeLine([]string{"mvn", "--version"}, local); a != "Apache Maven 3.10.0 (abc123)" || a != b {
		t.Fatalf("%q %q", a, b)
	}
	gradle := "\n------------------------------------------------------------\nGradle 9.8.2\n------------------------------------------------------------\n\nOS: Linux 6.8.0 amd64"
	if got := probeLine([]string{"gradle", "--version"}, gradle); got != "Gradle 9.8.2" {
		t.Fatalf("gradle: %q", got)
	}
	// Other tools, and output without the line, are fingerprinted whole.
	for _, c := range [][2]string{{"java", "openjdk 21.0.5\nOpenJDK Runtime"}, {"mvn", "Error: JAVA_HOME is not defined correctly."}} {
		if got := probeLine([]string{c[0], "--version"}, c[1]); got != c[1] {
			t.Errorf("%s: %q", c[0], got)
		}
	}
}
