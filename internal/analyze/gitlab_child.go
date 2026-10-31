package analyze

// The child pipeline cinema's GitLab job writes (plan §11.7): GitLab fixes
// rules: when it creates a pipeline, so they can't read receipts; instead the
// parent pipeline keeps the jobs cinema doesn't cover, cinema setup hides the
// ones it covers (.cinema:<job>), and the cinema job generates a child
// pipeline that runs exactly the covered jobs that still need to run.
//
// The child includes the repository's own .gitlab-ci.yml, so GitLab itself
// resolves includes, extends, !reference, default: and variables as for the
// parent. Every job of it is switched off, and each covered job that runs is
// a job extending its hidden copy, with its rules decided in the parent (in a
// child pipeline $CI_PIPELINE_SOURCE is parent_pipeline, so the job's own
// rules would be judged against the wrong pipeline).

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// GitLabConfig is a parsed .gitlab-ci.yml, for setup and the gate.
type GitLabConfig struct{ c *glConfig }

// LoadGitLab parses root's .gitlab-ci.yml (src, if not nil).
func LoadGitLab(root string, src []byte) (*GitLabConfig, error) {
	if src == nil {
		b, err := readRepoNoLinks(root, GitLabFile)
		if err != nil {
			return nil, err
		}
		src = b
	}
	c, err := loadGitLab(root, src)
	if err != nil {
		return nil, err
	}
	return &GitLabConfig{c}, nil
}

// DefaultBranch is the project's default branch, from origin/HEAD ("" if
// unknown).
func (g *GitLabConfig) DefaultBranch() string { return gitlabRepo(g.c.root).DefaultBranch }

// Stages are the pipeline's stages, in order.
func (g *GitLabConfig) Stages() []string { return slices.Clone(g.c.stages) }

// Unreadable says why cinema can't see the whole pipeline ("" if it can):
// a remote, project, template or component include, or a conditional one.
func (g *GitLabConfig) Unreadable() string { return strings.Join(g.c.reviewAll, "; ") }

// Tags are job's runner tags as resolved (default: included).
func (g *GitLabConfig) Tags(job string) []string {
	j := g.c.jobs[job]
	if j == nil {
		return nil
	}
	v, _ := j.get("tags")
	var out []string
	for _, t := range flatList(v) {
		out = append(out, str(t))
	}
	return out
}

// Dependents says, for each job or template, which jobs use it by name:
// extends, !reference, needs: or dependencies:. Setup won't hide those.
func (g *GitLabConfig) Dependents() map[string][]string {
	out := map[string][]string{}
	add := func(target, by string) {
		if target != by && !slices.Contains(out[target], by) {
			out[target] = append(out[target], by)
		}
	}
	var refs func(v any, by string)
	refs = func(v any, by string) {
		switch x := v.(type) {
		case gref:
			if len(x) > 0 {
				add(x[0], by)
			}
		case *gmap:
			for _, k := range x.keys {
				refs(x.m[k], by)
			}
		case []any:
			for _, e := range x {
				refs(e, by)
			}
		}
	}
	for name, j := range g.c.raw {
		ext, _ := j.get("extends")
		for _, e := range flatList(ext) {
			add(str(e), name)
		}
		for _, k := range []string{"needs", "dependencies"} {
			v, _ := j.get(k)
			for _, n := range flatList(v) {
				if m, ok := n.(*gmap); ok {
					jn, _ := m.get("job")
					add(str(jn), name)
				} else {
					add(str(n), name)
				}
			}
		}
		refs(j, name)
	}
	if def, ok := g.c.top.get("default"); ok {
		refs(def, "default")
	}
	refs(g.c.top.m["variables"], "variables")
	for k, v := range out {
		sort.Strings(v)
		out[k] = v
	}
	return out
}

// GitLabChildJob is a covered job the child pipeline runs.
type GitLabChildJob struct {
	Name string
	// Legs: the parallel: matrix: legs still to run (their variables); nil
	// runs the job whole.
	Legs []map[string]string
}

// GitLabChildPipeline is the generated child pipeline.
type GitLabChildPipeline struct {
	YAML []byte
	Runs []string // covered jobs it runs
	Off  []string // covered jobs whose rules keep them out of this pipeline
}

// Child writes the child pipeline running want, judged with env (the parent
// pipeline's variables, as the cinema job sees them). A job whose rules
// cinema can't decide runs (unknown means run).
func (g *GitLabConfig) Child(env func(string) (string, bool), want []GitLabChildJob) (GitLabChildPipeline, error) {
	var out GitLabChildPipeline
	m := func(kv ...*yaml.Node) *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Content: kv} }
	s := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
	q := func(v string) *yaml.Node {
		return &yaml.Node{Kind: yaml.ScalarNode, Value: v, Style: yaml.DoubleQuotedStyle}
	}
	seq := func(ns ...*yaml.Node) *yaml.Node { return &yaml.Node{Kind: yaml.SequenceNode, Content: ns} }
	real := RealEnv(env)
	root := m(s("include"), seq(m(s("local"), s(GitLabFile))))

	// The pipeline exists already (the parent passed workflow:); keep the
	// variables its matching workflow rule set.
	wf := m(s("rules"), seq(m(s("when"), s("always"))))
	if w, ok := g.c.top.get("workflow"); ok {
		if wr, ok := w.(*gmap).get("rules"); ok {
			d := evalRules(wr, g.envFor(nil, real), "", func(p []string) tri { return GitLabExists(g.c.root, p) })
			if d.vars != nil {
				wf.Content[1].Content[0].Content = append(wf.Content[1].Content[0].Content, s("variables"), glVarsNode(d.vars))
			}
		}
	}
	root.Content = append(root.Content, s("workflow"), wf)

	wanted := map[string]GitLabChildJob{}
	for _, w := range want {
		wanted[w.Name] = w
	}
	// Every job of the repository's pipeline is off in the child...
	for _, k := range g.c.top.keys {
		v, isMap := g.c.top.m[k].(*gmap)
		if glReserved[k] || !isMap || strings.HasPrefix(k, ".") {
			continue
		}
		_, only := v.get("only")
		_, except := v.get("except")
		_, rules := v.get("rules")
		if (only || except) && !rules {
			// rules: can't join only:/except:; an only: on a variable nothing sets never matches.
			root.Content = append(root.Content, q(k), m(s("only"), m(s("variables"), seq(s("$CINEMA_CHILD_NEVER")))))
			continue
		}
		root.Content = append(root.Content, q(k), m(s("rules"), seq(m(s("when"), s("never")))))
	}
	// ...but the covered jobs that still need to run.
	for _, w := range want {
		hidden := HiddenPrefix + w.Name
		raw := g.c.raw[hidden]
		j := g.c.jobs[w.Name]
		if raw == nil || j == nil {
			return out, fmt.Errorf("job %s isn't hidden as %s in %s (rerun cinema setup)", w.Name, hidden, GitLabFile)
		}
		d := glDecision{runs: triTrue, when: "on_success"}
		jobWhen := str(j.m["when"])
		if rules, ok := j.get("rules"); ok {
			d = evalRules(rules, g.envFor(j, real), jobWhen, func(p []string) tri { return GitLabExists(g.c.root, p) })
		} else {
			t, _ := onlyExcept(j, glScenario{source: cmpOr(lookup(env, "CI_PIPELINE_SOURCE"), "push")}, g.envFor(j, real))
			d = glDecision{runs: t, when: cmpOr(jobWhen, "on_success")}
		}
		if d.runs == triFalse {
			out.Off = append(out.Off, w.Name)
			continue
		}
		rule := m(s("when"), s(cmpOr(d.when, "on_success")))
		if d.runs == triUnknown {
			rule = m(s("when"), s("on_success")) // can't tell: it runs
		}
		if d.allowFailure != "" {
			rule.Content = append(rule.Content, s("allow_failure"), s(d.allowFailure))
		}
		if d.vars != nil {
			rule.Content = append(rule.Content, s("variables"), glVarsNode(d.vars))
		}
		job := m(s("extends"), q(hidden), s("rules"), seq(rule))
		_, only := j.get("only")
		_, except := j.get("except")
		if only || except {
			job.Content = append(job.Content, s("only"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"},
				s("except"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"})
		}
		if w.Legs != nil {
			mx := seq()
			for _, l := range w.Legs {
				e := m()
				for _, k := range sortedKeys(l) {
					e.Content = append(e.Content, s(k), q(l[k]))
				}
				mx.Content = append(mx.Content, e)
			}
			job.Content = append(job.Content, s("parallel"), m(s("matrix"), mx))
		}
		// Replace the off switch written above for the job's own name, if any.
		replaced := false
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == w.Name {
				root.Content[i+1], replaced = job, true
			}
		}
		if !replaced {
			root.Content = append(root.Content, q(w.Name), job)
		}
		out.Runs = append(out.Runs, w.Name)
	}
	if len(out.Runs) == 0 {
		// GitLab refuses to create an empty child pipeline, and the trigger job
		// would fail: one cheap job says why nothing else runs.
		stage := "test"
		if len(g.c.stages) > 0 {
			stage = g.c.stages[0]
		}
		root.Content = append(root.Content, q("cinema:proven"), m(
			s("stage"), q(stage), s("image"), s("busybox:1.37"), s("needs"), seq(),
			s("inherit"), m(s("default"), s("false"), s("variables"), s("false")),
			s("rules"), seq(m(s("when"), s("always"))),
			s("script"), seq(q("echo 'cinema: every covered job is proven by a signed local run (see the cinema job)'"))))
	}
	root.HeadComment = "Generated by cinema verify in the cinema job: the covered jobs that still need to run.\n" +
		"Everything else in " + GitLabFile + " runs in the parent pipeline. Not a file to edit."
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}); err != nil {
		return out, err
	}
	enc.Close()
	out.YAML = b.Bytes()
	return out, nil
}

func lookup(env func(string) (string, bool), k string) string {
	v, _ := env(k)
	return v
}

// envFor is the environment a job's rules see: the running pipeline's
// variables (project and predefined ones win), then the job's YAML ones.
func (g *GitLabConfig) envFor(j *gmap, real glEnv) glEnv {
	vars := newGmap()
	if gv, ok := g.c.top.get("variables"); ok {
		vars = glVarsOf(gv)
	}
	if j != nil {
		if jv, ok := j.get("variables"); ok {
			for k, v := range glVarsOf(jv).m {
				vars.set(k, v)
			}
		}
	}
	return func(n string) glVal {
		if v := real(n); v.state != vUnset {
			return v
		}
		if v, ok := vars.m[n]; ok {
			val, expands := glVarValue(v)
			if expands {
				val = glVarRef.ReplaceAllStringFunc(val, func(ref string) string {
					if ref == "$$" {
						return "$"
					}
					return real(strings.Trim(ref, "${}")).s
				})
			}
			return knownVal(val)
		}
		return glVal{state: vUnset}
	}
}

func glVarsNode(v *gmap) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode}
	for _, k := range v.keys {
		val, _ := glVarValue(v.m[k])
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, &yaml.Node{Kind: yaml.ScalarNode, Value: val, Style: yaml.DoubleQuotedStyle})
	}
	return n
}

// Hidden are the jobs cinema setup took out of the parent pipeline (.cinema:<job>):
// the child pipeline must run each of them unless it was proven.
func (g *GitLabConfig) Hidden() []string {
	var out []string
	for _, k := range g.c.top.keys {
		name, ok := strings.CutPrefix(k, HiddenPrefix)
		if _, isMap := g.c.top.m[k].(*gmap); !ok || !isMap {
			continue
		}
		if _, visible := g.c.top.m[name]; !visible {
			out = append(out, name)
		}
	}
	return out
}
