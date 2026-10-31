package analyze

// Reading .gitlab-ci.yml the way GitLab does before any job runs: local
// includes merged in (deep merge, the including file wins), extends resolved
// (multi-level, deep merge, the job wins), !reference tags replaced, default:
// and global variables applied. What cinema can't read (remote, project,
// template and component includes; conditional includes) is reported, so every
// job it could change goes to review: unknown means run.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"

	"go.yaml.in/yaml/v3"
)

// GitLabFile is the pipeline configuration GitLab reads by default (a project
// can name another file in its CI/CD settings; cinema reads this one).
const GitLabFile = ".gitlab-ci.yml"

// HiddenPrefix is how cinema setup takes a covered job out of the parent
// pipeline: the job's key gets this prefix, which makes it a hidden job (a
// template), and the generated child pipeline runs it through extends when it
// still needs to run. The analyzer reads such a job as the job it was.
const HiddenPrefix = ".cinema:"

// gmap is a YAML mapping that keeps its key order.
type gmap struct {
	keys []string
	m    map[string]any
}

func newGmap() *gmap { return &gmap{m: map[string]any{}} }

func (g *gmap) get(k string) (any, bool) {
	if g == nil {
		return nil, false
	}
	v, ok := g.m[k]
	return v, ok
}

func (g *gmap) set(k string, v any) {
	if _, ok := g.m[k]; !ok {
		g.keys = append(g.keys, k)
	}
	g.m[k] = v
}

func (g *gmap) del(k string) {
	if _, ok := g.m[k]; !ok {
		return
	}
	delete(g.m, k)
	g.keys = slices.DeleteFunc(g.keys, func(x string) bool { return x == k })
}

// gref is a !reference [job, key, ...] tag, resolved after extends.
type gref []string

// conv turns a parsed YAML node into gmap / []any / string / nil / gref,
// resolving anchors, aliases and << merge keys. budget caps the nodes visited,
// so an alias bomb can't exhaust memory.
func conv(n *yaml.Node, budget *int) (any, error) {
	if *budget--; *budget < 0 {
		return nil, errors.New("too large (aliases expand past 1,000,000 nodes)")
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return conv(n.Content[0], budget)
	case yaml.AliasNode:
		return conv(n.Alias, budget)
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil, nil
		}
		return n.Value, nil
	case yaml.SequenceNode:
		if n.Tag == "!reference" {
			var r gref
			for _, c := range n.Content {
				if c.Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("line %d: !reference takes a list of names", n.Line)
				}
				r = append(r, c.Value)
			}
			return r, nil
		}
		out := []any{}
		for _, c := range n.Content {
			v, err := conv(c, budget)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		g := newGmap()
		var merged []*gmap
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" || k.Value == "<<" && k.Style == 0 {
				vs := []*yaml.Node{v}
				if v.Kind == yaml.SequenceNode {
					vs = v.Content
				}
				for _, x := range vs {
					mv, err := conv(x, budget)
					if err != nil {
						return nil, err
					}
					if mm, ok := mv.(*gmap); ok {
						merged = append(merged, mm)
					}
				}
				continue
			}
			cv, err := conv(v, budget)
			if err != nil {
				return nil, err
			}
			g.set(k.Value, cv)
		}
		for _, mm := range merged { // the mapping's own keys win; then earlier merges
			for _, k := range mm.keys {
				if _, ok := g.m[k]; !ok {
					g.set(k, mm.m[k])
				}
			}
		}
		return g, nil
	}
	return nil, nil
}

// deepMerge is GitLab's deep_merge: mappings merge key by key (b wins),
// anything else in b replaces a.
func deepMerge(a, b any) any {
	am, aok := a.(*gmap)
	bm, bok := b.(*gmap)
	if !aok || !bok {
		return b
	}
	out := newGmap()
	for _, k := range am.keys {
		out.set(k, am.m[k])
	}
	for _, k := range bm.keys {
		if av, ok := out.m[k]; ok {
			out.set(k, deepMerge(av, bm.m[k]))
		} else {
			out.set(k, bm.m[k])
		}
	}
	return out
}

// glReserved are top-level keys that aren't jobs.
var glReserved = map[string]bool{"image": true, "services": true, "cache": true, "before_script": true, "after_script": true,
	"variables": true, "stages": true, "include": true, "workflow": true, "default": true, "types": true, "spec": true}

// glDefaultKeys are what default: (and the legacy top-level keys) give a job
// that doesn't set them itself.
var glDefaultKeys = []string{"after_script", "artifacts", "before_script", "cache", "hooks", "id_tokens", "identity", "image",
	"interruptible", "retry", "services", "tags", "timeout"}

// glConfig is a pipeline configuration as GitLab sees it after includes.
type glConfig struct {
	root      string
	top       *gmap
	jobs      map[string]*gmap // every visible job, fully resolved
	order     []string         // visible job names, in file order
	reviewAll []string         // why every job needs a human (unreadable includes)
	jobErr    map[string]string
	stages    []string
	includes  []string         // local files merged in (repo-relative)
	raw       map[string]*gmap // every job and template as written (after includes)
}

// loadGitLab reads root/.gitlab-ci.yml (or src, if given) and resolves it.
func loadGitLab(root string, src []byte) (*glConfig, error) {
	c := &glConfig{root: root, jobs: map[string]*gmap{}, jobErr: map[string]string{}}
	top, err := c.load(src, GitLabFile, 0, map[string]bool{})
	if err != nil {
		return nil, err
	}
	c.top = top
	c.stages = []string{"build", "test", "deploy"}
	if s, ok := top.get("stages"); ok {
		c.stages = nil
		for _, x := range flatList(s) {
			c.stages = append(c.stages, str(x))
		}
	}
	if _, ok := top.get("types"); ok {
		c.reviewAll = append(c.reviewAll, "the config uses types: (removed in GitLab 16): cinema reads stages:")
	}
	// Jobs as written, minus setup's own; a covered job setup hid reads as itself.
	raw := map[string]*gmap{}
	var visible []string
	for _, k := range top.keys {
		v, isMap := top.m[k].(*gmap)
		if glReserved[k] || !isMap {
			continue
		}
		raw[k] = v
		name := k
		if strings.HasPrefix(k, HiddenPrefix) {
			name = strings.TrimPrefix(k, HiddenPrefix)
			if _, dup := top.m[name]; dup {
				continue // the job exists as written too: the hidden copy is a template
			}
		} else if strings.HasPrefix(k, ".") {
			continue
		}
		if cinemaGitLabJob(k, v) {
			continue
		}
		visible = append(visible, k)
	}
	c.raw = raw
	extended := map[string]*gmap{}
	for k := range raw {
		j, err := extendJob(raw, k, 0, nil)
		if err != nil {
			c.jobErr[k] = err.Error()
			continue
		}
		extended[k] = j
	}
	def, _ := top.get("default")
	defm, _ := def.(*gmap)
	if defm != nil {
		r, err := resolveRefs(defm, extended, 0)
		if err != nil {
			return nil, fmt.Errorf("default: %v", err)
		}
		defm = r.(*gmap)
	}
	for _, k := range visible {
		name := strings.TrimPrefix(k, HiddenPrefix)
		if e, bad := c.jobErr[k]; bad {
			delete(c.jobErr, k)
			c.jobErr[name] = e
			c.order = append(c.order, name)
			continue
		}
		j, err := resolveRefs(extended[k], extended, 0)
		if err != nil {
			c.jobErr[name] = err.Error()
			c.order = append(c.order, name)
			continue
		}
		jm := j.(*gmap)
		applyDefaults(jm, defm, top)
		c.jobs[name] = jm
		c.order = append(c.order, name)
	}
	return c, nil
}

// cinemaGitLabJob: setup's cinema job or the trigger of its child pipeline.
func cinemaGitLabJob(k string, j *gmap) bool {
	if k != "cinema" && k != "cinema:child" {
		return false
	}
	if t, ok := j.get("trigger"); ok {
		inc, _ := t.(*gmap).get("include")
		for _, x := range flatList(inc) {
			if m, ok := x.(*gmap); ok {
				if v, _ := m.get("job"); v == "cinema" {
					return true
				}
			}
		}
		return false
	}
	s, _ := j.get("script")
	for _, l := range flatList(s) {
		if cinemaGate.MatchString(str(l)) || strings.Contains(str(l), " verify --gitlab-child ") {
			return true
		}
	}
	return false
}

// load reads one config file and everything it includes, merged.
func (c *glConfig) load(src []byte, name string, depth int, seen map[string]bool) (*gmap, error) {
	if depth > 100 || len(c.includes) > 150 {
		return nil, errors.New("too many nested includes (GitLab allows 150)")
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(strings.NewReader(string(src)))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return newGmap(), nil
		}
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	budget := 1_000_000
	v, err := conv(&doc, &budget)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", name, err)
	}
	top, ok := v.(*gmap)
	if v != nil && !ok {
		return nil, fmt.Errorf("%s: not a mapping", name)
	}
	if top == nil {
		top = newGmap()
	}
	if spec, ok := top.get("spec"); ok && len(top.keys) == 1 {
		// A header section (spec: inputs) and the config in a second document.
		_ = spec
		var next yaml.Node
		if err := dec.Decode(&next); err == nil {
			budget := 1_000_000
			v, err := conv(&next, &budget)
			if err != nil {
				return nil, fmt.Errorf("%s: %v", name, err)
			}
			if m, ok := v.(*gmap); ok {
				top = m
			}
		}
		c.reviewAll = append(c.reviewAll, name+" declares spec: inputs: cinema doesn't interpolate $[[ inputs ]]")
	}
	inc, ok := top.get("include")
	if !ok {
		return top, nil
	}
	top.del("include")
	var items []any
	switch x := inc.(type) {
	case []any:
		items = x
	default:
		items = []any{x}
	}
	merged := any(newGmap())
	for _, it := range items {
		var local string
		switch x := it.(type) {
		case string:
			if strings.HasPrefix(x, "http://") || strings.HasPrefix(x, "https://") {
				c.reviewAll = append(c.reviewAll, "includes remote file "+x+": cinema can't read it, and it can change any job")
				continue
			}
			local = x
		case *gmap:
			if _, ok := x.get("rules"); ok {
				c.reviewAll = append(c.reviewAll, "has a conditional include (include: rules:): cinema doesn't evaluate it")
				continue
			}
			if _, ok := x.get("inputs"); ok {
				c.reviewAll = append(c.reviewAll, "passes inputs: to an include: cinema doesn't interpolate them")
				continue
			}
			kind := ""
			for _, k := range []string{"remote", "project", "template", "component", "artifact"} {
				if _, ok := x.get(k); ok {
					kind = k
				}
			}
			if kind != "" {
				v, _ := x.get(kind)
				c.reviewAll = append(c.reviewAll, fmt.Sprintf("includes %s %s: cinema can't read it, and it can change any job", kind, str(v)))
				continue
			}
			l, _ := x.get("local")
			local = str(l)
		default:
			c.reviewAll = append(c.reviewAll, "has an include: entry cinema can't read")
			continue
		}
		if strings.Contains(local, "$") {
			c.reviewAll = append(c.reviewAll, "includes "+local+": a path with variables")
			continue
		}
		files, err := c.localFiles(local)
		if err != nil {
			return nil, fmt.Errorf("%s: include %s: %v", name, local, err)
		}
		for _, f := range files {
			if seen[f] {
				return nil, fmt.Errorf("%s: include %s: included twice or in a cycle", name, f)
			}
			seen[f] = true
			b, err := readRepoFile(c.root, f)
			if err != nil {
				return nil, fmt.Errorf("%s: include %s: %v", name, f, err)
			}
			c.includes = append(c.includes, f)
			sub, err := c.load(b, f, depth+1, seen)
			if err != nil {
				return nil, err
			}
			merged = deepMerge(merged, sub)
		}
	}
	return deepMerge(merged, top).(*gmap), nil
}

// localFiles expands a local include (a path from the repo root; * matches
// within a directory, ** across directories) to repo-relative file names.
func (c *glConfig) localFiles(p string) ([]string, error) {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	if c.root == "" {
		return nil, errors.New("no repository to read it from")
	}
	if !strings.Contains(p, "*") {
		return []string{p}, nil
	}
	var out []string
	err := filepath.WalkDir(c.root, func(f string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(c.root, f)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if globMatch(p, rel) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	if err == nil && len(out) == 0 {
		err = errors.New("matches no file")
	}
	return out, err
}

// globMatch: * and ? within a path segment, ** across segments.
func globMatch(pat, name string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; {
		case c == '*' && i+1 < len(pat) && pat[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	ok, _ := regexp.MatchString(b.String()+"$", name)
	return ok
}

// readRepoFile reads a repo-relative file the config names: no symlinks, a
// regular file, at most 1 MiB (the config is untrusted input).
func readRepoFile(root, rel string) ([]byte, error) {
	if !filepath.IsLocal(filepath.FromSlash(rel)) {
		return nil, errors.New("outside the repository")
	}
	f, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(rel)), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err == nil && len(b) > 1<<20 {
		err = errors.New("larger than 1 MiB")
	}
	return b, err
}

// extendJob resolves a job's extends: each base (resolved itself) merged in
// order, then the job's own keys on top. GitLab allows 11 levels.
func extendJob(raw map[string]*gmap, k string, depth int, path []string) (*gmap, error) {
	j := raw[k]
	if j == nil {
		return nil, fmt.Errorf("extends %s, which isn't defined", k)
	}
	if slices.Contains(path, k) {
		return nil, fmt.Errorf("extends itself (%s)", strings.Join(append(path, k), " → "))
	}
	if depth > 11 {
		return nil, errors.New("extends nests deeper than 11 levels")
	}
	ext, ok := j.get("extends")
	if !ok {
		return j, nil
	}
	var acc any = newGmap()
	for _, b := range flatList(ext) {
		base, err := extendJob(raw, str(b), depth+1, append(path, k))
		if err != nil {
			return nil, err
		}
		acc = deepMerge(acc, base)
	}
	own := newGmap()
	for _, key := range j.keys {
		if key != "extends" {
			own.set(key, j.m[key])
		}
	}
	return deepMerge(acc, own).(*gmap), nil
}

// resolveRefs replaces !reference tags in v with what they name in the
// extended jobs (or the top-level sections they're looked up in), up to 10
// levels deep. Script lists are flattened, as GitLab does with nested arrays.
func resolveRefs(v any, jobs map[string]*gmap, depth int) (any, error) {
	if depth > 10 {
		return nil, errors.New("!reference nests deeper than 10 levels")
	}
	switch x := v.(type) {
	case gref:
		if len(x) == 0 {
			return nil, errors.New("empty !reference")
		}
		var cur any = jobs[x[0]]
		if jobs[x[0]] == nil {
			return nil, fmt.Errorf("!reference %v: no job or template %s", []string(x), x[0])
		}
		for _, k := range x[1:] {
			m, ok := cur.(*gmap)
			if !ok {
				return nil, fmt.Errorf("!reference %v: %s isn't there", []string(x), k)
			}
			if cur, ok = m.get(k); !ok {
				return nil, fmt.Errorf("!reference %v: %s isn't there", []string(x), k)
			}
		}
		return resolveRefs(cur, jobs, depth+1)
	case *gmap:
		out := newGmap()
		for _, k := range x.keys {
			r, err := resolveRefs(x.m[k], jobs, depth)
			if err != nil {
				return nil, err
			}
			if k == "script" || k == "before_script" || k == "after_script" {
				r = flatScript(r)
			}
			out.set(k, r)
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			r, err := resolveRefs(e, jobs, depth)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
	return v, nil
}

func flatScript(v any) any {
	l, ok := v.([]any)
	if !ok {
		return v
	}
	var out []any
	for _, e := range l {
		if sub, ok := e.([]any); ok {
			out = append(out, flatScript(sub).([]any)...)
		} else {
			out = append(out, e)
		}
	}
	if out == nil {
		out = []any{}
	}
	return out
}

// applyDefaults gives job j what default: (or the legacy top-level keys) sets
// and j doesn't, honoring inherit: default:, and the global variables under
// inherit: variables:. Variables end up as one mapping (job wins).
func applyDefaults(j, def, top *gmap) {
	inheritDef, inheritVars := any("true"), any("true")
	if inh, ok := j.get("inherit"); ok {
		if m, ok := inh.(*gmap); ok {
			if v, ok := m.get("default"); ok {
				inheritDef = v
			}
			if v, ok := m.get("variables"); ok {
				inheritVars = v
			}
		}
	}
	allowed := func(spec any, k string) bool {
		switch s := spec.(type) {
		case string:
			return s != "false"
		case []any:
			return slices.ContainsFunc(s, func(x any) bool { return str(x) == k })
		}
		return true
	}
	for _, k := range glDefaultKeys {
		if _, has := j.get(k); has || !allowed(inheritDef, k) {
			continue
		}
		if v, ok := def.get(k); ok {
			j.set(k, v)
		} else if v, ok := top.get(k); ok && k != "variables" {
			j.set(k, v) // legacy top-level image:, services:, before_script:…
		}
	}
	vars := newGmap()
	if g, ok := top.get("variables"); ok {
		if gm, ok := g.(*gmap); ok {
			for _, k := range gm.keys {
				if allowed(inheritVars, k) {
					vars.set(k, gm.m[k])
				}
			}
		}
	}
	if own, ok := j.get("variables"); ok {
		if om, ok := own.(*gmap); ok {
			for _, k := range om.keys {
				vars.set(k, om.m[k])
			}
		}
	}
	j.set("variables", vars)
}

// flatList is v as a list: a scalar is a list of one, nil is empty.
func flatList(v any) []any {
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		return x
	}
	return []any{v}
}

// plain turns the tree into encoding/json-friendly values (maps sort their
// keys), for the job definition's digest.
func plain(v any) any {
	switch x := v.(type) {
	case *gmap:
		m := make(map[string]any, len(x.keys))
		for _, k := range x.keys {
			m[k] = plain(x.m[k])
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case gref:
		return []string(x)
	}
	return v
}
