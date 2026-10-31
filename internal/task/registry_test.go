package task

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitRef(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":                             "registry-1.docker.io library/alpine latest",
		"golang:1.26":                        "registry-1.docker.io library/golang 1.26",
		"bitnami/redis:7":                    "registry-1.docker.io bitnami/redis 7",
		"docker.io/library/node:22":          "registry-1.docker.io library/node 22",
		"registry.gitlab.com/g/p/img:v1":     "registry.gitlab.com g/p/img v1",
		"localhost:5000/x":                   "localhost:5000 x latest",
		"ghcr.io/owner/image:1.2.3-bookworm": "ghcr.io owner/image 1.2.3-bookworm",
	} {
		h, r, tag := splitRef(in)
		if got := h + " " + r + " " + tag; got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A registry that wants an anonymous Bearer token, as Docker Hub does.
func TestRegistryDigest(t *testing.T) {
	const digest = "sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:library/alpine:pull" {
				http.Error(w, "scope", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"token":"t0k"}`)
		case r.URL.Path == "/v2/library/alpine/manifests/3.20":
			if r.Header.Get("Authorization") != "Bearer t0k" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "manifest.list.v2+json") {
				http.Error(w, "accept", http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Docker-Content-Digest", digest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldScheme := registryScheme
	defer func() { registryScheme = oldScheme }()
	registryScheme = "http"
	allowPrivateRegistry = true
	defer func() { allowPrivateRegistry = false }()
	host := strings.TrimPrefix(srv.URL, "http://")
	got, err := registryDigest(host + "/library/alpine:3.20")
	if err != nil || got != digest {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := registryDigest(host + "/library/missing:1"); err == nil {
		t.Error("a missing image resolved")
	}
}

// Image names and the auth realm come from the config under test: nothing may
// reach the runner's own network, and the realm must be the registry's scheme.
func TestRegistryRefusesPrivateAndBadRealm(t *testing.T) {
	for _, a := range []string{"127.0.0.1:443", "10.0.0.5:443", "169.254.169.254:80", "[::1]:443", "0.0.0.0:443",
		"100.64.1.1:443", "198.18.0.1:443", "240.0.0.1:443", "[::ffff:169.254.169.254]:80", "[64:ff9b::a9fe:a9fe]:80",
		"[2002:a9fe:a9fe::1]:80", "[2002:0a00:0001::1]:443", "[fd00::1]:443", "[fe80::1]:443"} {
		if refusePrivate("tcp", a, nil) == nil {
			t.Errorf("%s allowed", a)
		}
	}
	for _, a := range []string{"140.82.112.3:443", "[2606:4700::6810:84e5]:443", "[64:ff9b::8c52:7003]:443"} {
		if err := refusePrivate("tcp", a, nil); err != nil {
			t.Errorf("public address %s refused: %v", a, err)
		}
	}
	if err := refusePrivate("tcp", "140.82.112.3:443", nil); err != nil {
		t.Errorf("public address refused: %v", err)
	}
	for _, realm := range []string{"http://auth.example.com/token", "file:///etc/passwd", "https://u:p@auth.example.com/t", "https://auth.example.com/t?x=1"} {
		if _, err := registryToken(`Bearer realm="`+realm+`",service="x"`, "lib/a"); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("realm %s: %v", realm, err)
		}
	}
}
