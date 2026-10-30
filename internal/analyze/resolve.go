package analyze

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// fetch is the HTTP GET used to list releases (a variable so tests can fake it).
var fetch = func(url string) ([]byte, error) {
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

type release struct {
	Version string
	LTS     bool
	Stable  bool
}

var releases = struct {
	sync.Mutex
	m map[string][]release
}{m: map[string][]release{}}

// list returns tool's releases, newest first, fetched once per process.
func list(tool string) ([]release, error) {
	releases.Lock()
	defer releases.Unlock()
	if r, ok := releases.m[tool]; ok {
		return r, nil
	}
	var out []release
	switch tool {
	case "Node":
		b, err := fetch("https://nodejs.org/dist/index.json")
		if err != nil {
			return nil, err
		}
		var v []struct {
			Version string `json:"version"`
			LTS     any    `json:"lts"` // false or a codename
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		for _, r := range v {
			lts, _ := r.LTS.(string)
			out = append(out, release{Version: strings.TrimPrefix(r.Version, "v"), LTS: lts != "", Stable: true})
		}
	case "Go":
		b, err := fetch("https://go.dev/dl/?mode=json&include=all")
		if err != nil {
			return nil, err
		}
		var v []struct {
			Version string `json:"version"`
			Stable  bool   `json:"stable"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		for _, r := range v {
			out = append(out, release{Version: strings.TrimPrefix(r.Version, "go"), Stable: r.Stable})
		}
	}
	releases.m[tool] = out
	return out, nil
}

// resolve maps a setup-go/setup-node version spec to the newest matching
// stable release, as those actions do at run time: "22", "22.x", "22.23",
// "lts/*", "1.26", "1.26.x", "stable". "" if the spec isn't understood.
func resolve(tool, spec string) (string, error) { return Resolve(tool, spec) }

// Resolve is resolve for the gate: it re-resolves a task's floating: specs at
// verify time, so a pin from yesterday can't stand in for what CI uses today.
func Resolve(tool, spec string) (string, error) {
	spec = strings.TrimSpace(strings.TrimPrefix(spec, "v"))
	rs, err := list(tool)
	if err != nil {
		return "", err
	}
	match := func(r release) bool {
		switch spec {
		case "lts/*", "lts":
			return tool == "Node" && r.LTS
		case "stable", "latest", "node", "current":
			return r.Stable
		}
		p := strings.TrimSuffix(strings.TrimSuffix(spec, ".x"), ".*")
		return r.Stable && (r.Version == p || strings.HasPrefix(r.Version, p+"."))
	}
	for _, r := range rs { // newest first
		if match(r) {
			return r.Version, nil
		}
	}
	return "", nil
}
