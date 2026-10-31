package analyze

// Evaluating GitLab's rules:, only:/except: and workflow: rules. Values can be
// unknown (a branch name, a project's CI/CD variable): comparisons with them
// are unknown unless they can be decided anyway (an unset variable never
// equals a string; a feature branch is never the default branch), and an
// unknown decision sends the job to review.

import (
	"regexp"
	"slices"
	"strings"
)

type tri int8

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func triOf(b bool) tri {
	if b {
		return triTrue
	}
	return triFalse
}

func (t tri) not() tri {
	switch t {
	case triTrue:
		return triFalse
	case triFalse:
		return triTrue
	}
	return triUnknown
}

// Variable states.
const (
	vUnset      = iota // not defined (null)
	vSet               // defined, value s
	vSetUnknown        // defined and non-empty, value unknown (except: never one of not)
	vUnknown           // maybe defined, value unknown
)

type glVal struct {
	state int8
	s     string
	not   []string
}

// glEnv looks a variable up for rule evaluation.
type glEnv func(name string) glVal

func knownVal(s string) glVal { return glVal{state: vSet, s: s} }

// RealEnv is the environment of a running job (the cinema job in the parent
// pipeline): every variable is known, an absent one is unset.
func RealEnv(get func(string) (string, bool)) glEnv {
	return func(n string) glVal {
		if v, ok := get(n); ok {
			return knownVal(v)
		}
		return glVal{state: vUnset}
	}
}

var glExprToken = regexp.MustCompile(`\s*(\$\{?[A-Za-z_][A-Za-z0-9_]*\}?|"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|/(?:[^/\\]|\\.)*/[a-z]*|==|!=|=~|!~|&&|\|\||\(|\)|null\b)`)

// glExpr evaluates a rules: if: expression.
func glExpr(src string, env glEnv) tri {
	var toks []string
	rest := strings.TrimSpace(src)
	for rest != "" {
		m := glExprToken.FindStringSubmatchIndex(rest)
		if m == nil || m[0] != 0 {
			return triUnknown // something cinema doesn't parse
		}
		toks = append(toks, rest[m[2]:m[3]])
		rest = strings.TrimSpace(rest[m[1]:])
	}
	p := &glParser{toks: toks, env: env}
	v := p.or()
	if p.bad || p.i != len(toks) {
		return triUnknown
	}
	return v
}

type glParser struct {
	toks []string
	i    int
	env  glEnv
	bad  bool
}

func (p *glParser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *glParser) or() tri {
	l := p.and()
	for p.peek() == "||" {
		p.i++
		r := p.and()
		switch {
		case l == triTrue || r == triTrue:
			l = triTrue
		case l == triUnknown || r == triUnknown:
			l = triUnknown
		default:
			l = triFalse
		}
	}
	return l
}

func (p *glParser) and() tri {
	l := p.cmp()
	for p.peek() == "&&" {
		p.i++
		r := p.cmp()
		switch {
		case l == triFalse || r == triFalse:
			l = triFalse
		case l == triUnknown || r == triUnknown:
			l = triUnknown
		default:
			l = triTrue
		}
	}
	return l
}

func (p *glParser) cmp() tri {
	if p.peek() == "(" {
		p.i++
		v := p.or()
		if p.peek() != ")" {
			p.bad = true
		}
		p.i++
		return v
	}
	l := p.operand()
	switch op := p.peek(); op {
	case "==", "!=":
		p.i++
		r := p.operand()
		v := glEqual(l, r)
		if op == "!=" {
			v = v.not()
		}
		return v
	case "=~", "!~":
		p.i++
		var re glVal
		if t := p.peek(); strings.HasPrefix(t, "/") {
			p.i++
			re = knownVal(t)
		} else {
			re = p.operand()
		}
		v := glMatch(l, re)
		if op == "!~" {
			v = v.not()
		}
		return v
	}
	switch l.state { // a bare variable: defined and not empty
	case vUnset:
		return triFalse
	case vSet:
		return triOf(l.s != "")
	case vSetUnknown:
		return triTrue
	}
	return triUnknown
}

func (p *glParser) operand() glVal {
	t := p.peek()
	p.i++
	switch {
	case t == "null":
		return glVal{state: vUnset}
	case strings.HasPrefix(t, "$"):
		return p.env(strings.Trim(t, "${}"))
	case strings.HasPrefix(t, `"`), strings.HasPrefix(t, "'"):
		s := t[1 : len(t)-1]
		s = strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\\`, `\`).Replace(s)
		return knownVal(s)
	}
	p.bad = true
	return glVal{state: vUnknown}
}

func glEqual(a, b glVal) tri {
	if a.state > b.state {
		a, b = b, a
	}
	switch {
	case a.state == vUnset:
		switch b.state {
		case vUnset:
			return triTrue
		case vSet, vSetUnknown:
			return triFalse
		}
		return triUnknown
	case a.state == vSet && b.state == vSet:
		return triOf(a.s == b.s)
	case a.state == vSet && b.state == vSetUnknown && slices.Contains(b.not, a.s):
		return triFalse
	}
	return triUnknown
}

func glMatch(l, re glVal) tri {
	if re.state != vSet {
		return triUnknown
	}
	pat := re.s
	if !strings.HasPrefix(pat, "/") || strings.LastIndex(pat, "/") == 0 {
		return triUnknown
	}
	end := strings.LastIndex(pat, "/")
	flags, body := pat[end+1:], pat[1:end]
	if strings.Contains(flags, "i") {
		body = "(?i)" + body
	}
	rx, err := regexp.Compile(body)
	if err != nil {
		return triUnknown
	}
	switch l.state {
	case vUnset:
		return triFalse
	case vSet:
		return triOf(rx.MatchString(l.s))
	}
	return triUnknown
}

// glDecision is whether a job (or a pipeline) runs, as its rules decide.
type glDecision struct {
	runs         tri
	when         string // on_success, manual, delayed, always, on_failure
	allowFailure string // the rule's allow_failure, "" if it sets none
	vars         *gmap  // the matching rule's variables
	why          string // why the decision is unknown
	rule         int    // index of the rule that decided, -1 if none
}

// evalRules is the first matching rule's outcome. exists decides a rule's
// exists: list (nil: unknown).
func evalRules(rules any, env glEnv, jobWhen string, exists func([]string) tri) glDecision {
	list := flatList(rules)
	for i, r := range list {
		m, ok := r.(*gmap)
		if !ok {
			return glDecision{runs: triUnknown, why: "a rule cinema can't read"}
		}
		cond := triTrue
		for _, k := range m.keys {
			switch k {
			case "if":
				cond = and3(cond, glExpr(str(m.m[k]), env))
			case "changes":
				cond = and3(cond, triUnknown)
			case "exists":
				var pats []string
				v := m.m[k]
				if em, ok := v.(*gmap); ok { // exists: {paths: [...], project:…}
					if _, other := em.get("project"); other {
						cond = and3(cond, triUnknown)
						continue
					}
					v, _ = em.get("paths")
				}
				for _, x := range flatList(v) {
					pats = append(pats, str(x))
				}
				if exists == nil {
					cond = and3(cond, triUnknown)
				} else {
					cond = and3(cond, exists(pats))
				}
			case "when", "allow_failure", "variables", "start_in", "needs", "interruptible", "auto_cancel":
			default:
				cond = and3(cond, triUnknown)
			}
		}
		switch cond {
		case triFalse:
			continue
		case triUnknown:
			why := "rules: " + ruleText(m)
			if _, ok := m.get("changes"); ok {
				why = "rules: changes: (which files a push or merge request changed) aren't known ahead of the pipeline"
			}
			return glDecision{runs: triUnknown, why: why, rule: i}
		}
		d := glDecision{runs: triTrue, when: cmpOr(str(m.m["when"]), jobWhen, "on_success"), rule: i}
		if d.when == "never" {
			d.runs = triFalse
		}
		if v, ok := m.get("allow_failure"); ok {
			d.allowFailure = str(v)
		}
		if v, ok := m.get("variables"); ok {
			d.vars, _ = v.(*gmap)
		}
		return d
	}
	return glDecision{runs: triFalse, when: "never", rule: -1}
}

func and3(a, b tri) tri {
	switch {
	case a == triFalse || b == triFalse:
		return triFalse
	case a == triUnknown || b == triUnknown:
		return triUnknown
	}
	return triTrue
}

func ruleText(m *gmap) string {
	if v, ok := m.get("if"); ok {
		return "if: " + str(v)
	}
	return strings.Join(m.keys, ", ")
}

func cmpOr(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// glScenario is what cinema assumes about a pipeline it drafts for, by
// source: a merge request pipeline (merge_request_event), or a branch
// pipeline for a push to a feature branch with no open merge request (push).
type glScenario struct {
	source        string
	defaultBranch string // "" if not known
	project       map[string]string
}

func (s glScenario) env(yamlVars map[string]glVal) glEnv {
	feature := glVal{state: vSetUnknown}
	if s.defaultBranch != "" {
		feature.not = []string{s.defaultBranch}
	}
	return func(n string) glVal {
		if v, ok := yamlVars[n]; ok {
			return v
		}
		if v, ok := s.project[n]; ok {
			return knownVal(v)
		}
		mr := s.source == "merge_request_event"
		switch {
		case n == "CI" || n == "GITLAB_CI":
			return knownVal("true")
		case n == "CI_PIPELINE_SOURCE":
			return knownVal(s.source)
		case n == "CI_DEFAULT_BRANCH":
			if s.defaultBranch == "" {
				return glVal{state: vSetUnknown}
			}
			return knownVal(s.defaultBranch)
		case n == "CI_COMMIT_TAG", n == "CI_COMMIT_TAG_MESSAGE", n == "CI_EXTERNAL_PULL_REQUEST_IID":
			return glVal{state: vUnset}
		case n == "CI_COMMIT_BRANCH":
			if mr {
				return glVal{state: vUnset}
			}
			return feature
		case n == "CI_COMMIT_REF_NAME", n == "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME" && mr:
			return feature
		case n == "CI_OPEN_MERGE_REQUESTS" && !mr:
			return glVal{state: vUnset} // the push scenario: no merge request is open
		case strings.HasPrefix(n, "CI_MERGE_REQUEST_") && !mr:
			return glVal{state: vUnset}
		case n == "CI_MERGE_REQUEST_ID", n == "CI_MERGE_REQUEST_IID", n == "CI_MERGE_REQUEST_EVENT_TYPE",
			n == "CI_MERGE_REQUEST_TARGET_BRANCH_NAME", n == "CI_MERGE_REQUEST_PROJECT_ID", n == "CI_MERGE_REQUEST_SOURCE_PROJECT_ID",
			n == "CI_COMMIT_SHA", n == "CI_COMMIT_SHORT_SHA", n == "CI_PIPELINE_ID", n == "CI_JOB_ID":
			return glVal{state: vSetUnknown}
		}
		return glVal{state: vUnknown} // a CI/CD variable from the project's settings, or one cinema doesn't model
	}
}

// onlyExcept evaluates only:/except: (GitLab's older syntax) for a scenario.
// Without either, a job runs in branch and tag pipelines, not merge request ones.
func onlyExcept(j *gmap, sc glScenario, env glEnv) (tri, string) {
	only, hasOnly := j.get("only")
	except, hasExcept := j.get("except")
	if !hasOnly {
		only = []any{"branches", "tags"}
	}
	in, why := refsMatch(only, sc, env, false)
	if in == triFalse || !hasExcept {
		return in, why
	}
	out, why2 := refsMatch(except, sc, env, true)
	return and3(in, out.not()), cmpOr(why, why2)
}

// refsMatch: does a only:/except: value match this pipeline? In the mapping
// form, only: needs every condition to hold, except: excludes on any.
func refsMatch(v any, sc glScenario, env glEnv, except bool) (tri, string) {
	mr := sc.source == "merge_request_event"
	m, ok := v.(*gmap)
	if !ok {
		return refList(v, mr, sc)
	}
	if _, ok := m.get("changes"); ok {
		return triUnknown, "only:/except: changes: (which files a push changed) aren't known ahead of the pipeline"
	}
	if _, ok := m.get("kubernetes"); ok {
		return triUnknown, "only:/except: kubernetes: depends on the project's cluster"
	}
	var conds []tri
	if refs, ok := m.get("refs"); ok {
		r, why := refList(refs, mr, sc)
		if why != "" {
			return triUnknown, why
		}
		conds = append(conds, r)
	}
	if vs, ok := m.get("variables"); ok {
		anyTrue := triFalse
		for _, e := range flatList(vs) {
			switch glExpr(str(e), env) {
			case triTrue:
				anyTrue = triTrue
			case triUnknown:
				if anyTrue != triTrue {
					anyTrue = triUnknown
				}
			}
		}
		conds = append(conds, anyTrue)
	}
	res := triOf(!except)
	for i, c := range conds {
		switch {
		case i == 0:
			res = c
		case except:
			res = and3(res.not(), c.not()).not() // or
		default:
			res = and3(res, c)
		}
	}
	return res, ""
}

func refList(refs any, mr bool, sc glScenario) (tri, string) {
	res := triFalse
	for _, x := range flatList(refs) {
		r := str(x)
		var m tri
		switch {
		case r == "merge_requests":
			m = triOf(mr)
		case r == "branches", r == "pushes":
			m = triOf(!mr)
		case r == "tags", r == "api", r == "external", r == "pipelines", r == "schedules", r == "triggers", r == "web", r == "chat", r == "external_pull_requests":
			m = triFalse
		case mr:
			m = triFalse // only merge_requests puts a job in a merge request pipeline
		case strings.Contains(r, "@"):
			return triUnknown, "only:/except: " + r + " names a project"
		case strings.HasPrefix(r, "/"):
			return triUnknown, "only:/except: " + r + " matches branch names, which cinema can't know ahead"
		case sc.defaultBranch != "" && r == sc.defaultBranch:
			m = triFalse // the scenario is a feature branch
		default:
			return triUnknown, "only:/except: " + r + " names a branch"
		}
		if m == triTrue {
			res = triTrue
		}
	}
	return res, ""
}
