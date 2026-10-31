package task

// Resolving an image tag to its digest straight from the registry (OCI
// distribution API), for machines without a Docker daemon: a GitLab job runs
// inside a container, so the cinema job there has no docker to ask (plan
// §11.2: "image by tag → a registry HEAD"). Anonymous pulls only. The digest
// is the one `docker pull` records in RepoDigests for the same tag: the
// manifest list's for multi-arch images, the manifest's otherwise.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	// The image names come from the CI config under test (a merge request's),
	// and so does the auth realm, from the registry's challenge: requests must
	// not reach the runner's own network (cloud metadata, internal services).
	registryClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, Control: refusePrivate}).DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != registryScheme || len(via) > 5 {
				return fmt.Errorf("registry redirect to %s refused", req.URL.Redacted())
			}
			return nil
		},
	}
	registryScheme       = "https" // a var for tests
	allowPrivateRegistry = false   // tests: a registry on 127.0.0.1
	// haveDocker: is there a docker CLI to ask (a var for tests).
	haveDocker = func() bool { _, err := exec.LookPath("docker"); return err == nil }
)

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.docker.distribution.manifest.v2+json, application/vnd.oci.image.manifest.v1+json"

var bearerParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

// splitRef is an image reference's registry host, repository and tag.
func splitRef(ref string) (host, repo, tag string) {
	name, tag := ref, "latest"
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		name, tag = name[:i], name[i+1:]
	}
	host = "registry-1.docker.io"
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, name = first, rest
		if host == "docker.io" || host == "index.docker.io" {
			host = "registry-1.docker.io"
		}
	}
	if host == "registry-1.docker.io" && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return host, name, tag
}

// registryDigest resolves ref (no @sha256) to sha256:… with the registry.
func registryDigest(ref string) (string, error) {
	host, repo, tag := splitRef(ref)
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", registryScheme, host, repo, url.PathEscape(tag))
	do := func(method, token string) (*http.Response, error) {
		req, err := http.NewRequest(method, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", manifestAccept)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return registryClient.Do(req)
	}
	resp, err := do(http.MethodHead, "")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	token := ""
	if resp.StatusCode == http.StatusUnauthorized {
		if token, err = registryToken(resp.Header.Get("WWW-Authenticate"), repo); err != nil {
			return "", err
		}
		if resp, err = do(http.MethodHead, token); err != nil {
			return "", err
		}
		resp.Body.Close()
	}
	if resp.StatusCode == http.StatusOK {
		if d := resp.Header.Get("Docker-Content-Digest"); strings.HasPrefix(d, "sha256:") && len(d) == 71 {
			return d, nil
		}
	}
	// Some registries don't answer HEAD with the digest: hash the manifest.
	if resp, err = do(http.MethodGet, token); err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry %s: %s for %s:%s", host, resp.Status, repo, tag)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); strings.HasPrefix(d, "sha256:") && len(d) == 71 {
		return d, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(resp.Body, 4<<20)); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// registryToken gets an anonymous pull token from a Bearer challenge.
func registryToken(challenge, repo string) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("registry wants %q auth: cinema resolves public images only", scheme)
	}
	p := map[string]string{}
	for _, m := range bearerParam.FindAllStringSubmatch(params, -1) {
		p[m[1]] = m[2]
	}
	if p["realm"] == "" {
		return "", errors.New("registry auth challenge without a realm")
	}
	if u, err := url.Parse(p["realm"]); err != nil || u.Scheme != registryScheme || u.Host == "" || u.RawQuery != "" || u.User != nil {
		return "", fmt.Errorf("registry auth realm %q refused: %s URLs only", p["realm"], registryScheme)
	}
	q := url.Values{}
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	q.Set("scope", cmpOrStr(p["scope"], "repository:"+repo+":pull"))
	resp, err := registryClient.Get(p["realm"] + "?" + q.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry token: %s", resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", err
	}
	return cmpOrStr(t.Token, t.AccessToken), nil
}

// refusePrivate refuses connections to addresses a public registry never has:
// loopback, private, link-local (cloud metadata), CGNAT, special-use, and IPv4
// of those kinds embedded in IPv6 (mapped, NAT64, 6to4).
func refusePrivate(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("registry address %s: %v", address, err)
	}
	if ip := ap.Addr(); !allowPrivateRegistry && !publicAddr(ip) {
		return fmt.Errorf("registry address %s refused: not a public address", ip)
	}
	return nil
}

var specialUse = func() (ps []netip.Prefix) {
	for _, s := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24",
		"203.0.113.0/24", "240.0.0.0/4", "64:ff9b:1::/48", "2001:db8::/32", "2001::/23"} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return
}()

func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || !ip.IsValid() {
		return false
	}
	for _, p := range specialUse {
		if p.Contains(ip) {
			return false
		}
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case netip.MustParsePrefix("64:ff9b::/96").Contains(ip): // NAT64: the IPv4 is the last 4 bytes
			return publicAddr(netip.AddrFrom4([4]byte(b[12:16])))
		case netip.MustParsePrefix("2002::/16").Contains(ip): // 6to4: the IPv4 is bytes 2..5
			return publicAddr(netip.AddrFrom4([4]byte(b[2:6])))
		}
	}
	return true
}

func cmpOrStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
