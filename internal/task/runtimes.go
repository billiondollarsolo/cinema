package task

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// RuntimeKind is a language runtime cinema provisions beyond Go and Node
// (plan §6.5): one file per language registers it with RegisterRuntime.
// Provisioning downloads the official release for the task's OS and arch
// (linux/amd64 on a Mac, for a sandbox), verifies it against the publisher's
// checksum, and unpacks it under <data>/runtimes, where sandboxes see it
// read-only at /opt/cinema/runtimes.
type RuntimeKind struct {
	Name    string         // the runtime: key ("python")
	Example string         // an exact version, for errors ("3.12.7")
	Version *regexp.Regexp // exact versions only: a range is a floating: spec, resolved by analyze
	// Provision installs ver for osName/arch under <dataDir>/runtimes (once;
	// later calls find it) and returns the install's root directory.
	Provision func(dataDir, ver, osName, arch string) (root string, err error)
	Bins      []string // dirs under root that go on PATH, in order ("bin")
	// Vars: environment the install needs, with root as the task sees it
	// (JAVA_HOME=root). nil: none.
	Vars func(root string) []string
	// Caches: download caches a sandbox keeps per repo: volume suffix → path
	// under the runner's home ("m2": ".m2/repository"). Content-keyed, never results.
	Caches map[string]string
	Probe  []string // the version command drafts fingerprint it with ([python3, --version])
}

var runtimeKinds = map[string]*RuntimeKind{}

// RegisterRuntime adds a runtime kind (from an init function).
func RegisterRuntime(k *RuntimeKind) {
	if k.Name == "go" || k.Name == "node" || runtimeKinds[k.Name] != nil {
		panic("runtime " + k.Name + " registered twice")
	}
	runtimeKinds[k.Name] = k
}

// LookupRuntime is the registered kind for a runtime: key, or nil.
func LookupRuntime(name string) *RuntimeKind { return runtimeKinds[name] }

// RuntimeNames is every runtime: key cinema provisions, sorted.
func RuntimeNames() []string {
	names := []string{"go", "node"}
	for n := range runtimeKinds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Get is the pinned version of runtime name ("" if not pinned).
func (r Runtime) Get(name string) string {
	switch name {
	case "go":
		return r.Go
	case "node":
		return r.Node
	}
	return r.Other[name]
}

// Set reports whether any runtime is pinned.
func (r Runtime) Set() bool { return r.Go != "" || r.Node != "" || len(r.Other) > 0 }

func (r Runtime) validateOther() error {
	for _, n := range sortedKeys(r.Other) {
		k, v := runtimeKinds[n], r.Other[n]
		switch {
		case k == nil:
			return fmt.Errorf("runtime.%s: cinema doesn't provision %s (it does: %s)", n, n, strings.Join(RuntimeNames(), ", "))
		case !k.Version.MatchString(v):
			return fmt.Errorf("runtime.%s must be an exact version like %s, got %q", n, k.Example, v)
		}
	}
	return nil
}

// otherInstalls provisions the registered runtimes t pins, in name order.
func (t *Task) otherInstalls(dataDir, osName, arch string) (bins, vars []string, caches map[string]string, err error) {
	for _, n := range sortedKeys(t.Runtime.Other) {
		k, v := runtimeKinds[n], t.Runtime.Other[n]
		if k == nil {
			return nil, nil, nil, fmt.Errorf("runtime %s: not provisioned by cinema", n)
		}
		root, err := k.Provision(dataDir, v, osName, arch)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s %s: %w", n, v, err)
		}
		if rel, err := filepath.Rel(filepath.Join(dataDir, "runtimes"), root); err != nil || strings.HasPrefix(rel, "..") {
			return nil, nil, nil, fmt.Errorf("%s %s: installed outside %s", n, v, filepath.Join(dataDir, "runtimes"))
		}
		for _, b := range k.Bins {
			bins = append(bins, filepath.Join(root, filepath.FromSlash(b)))
		}
		if k.Vars != nil {
			vars = append(vars, k.Vars(root)...)
		}
		for vol, p := range k.Caches {
			if caches == nil {
				caches = map[string]string{}
			}
			caches[vol] = p
		}
	}
	return bins, vars, caches, nil
}

// RuntimeSandbox is RuntimeBins for a sandbox: also the variables the
// installs need (host paths under <data>/runtimes, which the sandbox maps to
// where it mounts them) and their download caches.
func (t *Task) RuntimeSandbox(dataDir, osName, arch string) (bins, vars []string, caches map[string]string, err error) {
	if bins, err = t.RuntimeBins(dataDir, osName, arch); err != nil {
		return nil, nil, nil, err
	}
	ob, vars, caches, err := t.otherInstalls(dataDir, osName, arch)
	if err != nil {
		return nil, nil, nil, err
	}
	return slices.Concat(ob, bins), vars, caches, nil
}
