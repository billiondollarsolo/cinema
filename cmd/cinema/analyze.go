package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
	"github.com/billiondollarsolo/cinema/internal/task"
)

// cmdAnalyze proposes tasks from the repo's GitHub Actions workflows. Advisory
// only: it prints, never writes tasks.yaml. A proposal is checked by running it
// (`cinema run`) and comparing with CI, not by reading it.
func cmdAnalyze(args []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	asYAML := fs.Bool("yaml", false, "print proposed tasks as a tasks.yaml draft")
	asJSON := fs.Bool("json", false, "machine-readable result")
	withMinutes := fs.Bool("minutes", false, "price each job from the repo's recent PR runs (median billed minutes, via the gh CLI)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: cinema analyze [--yaml | --json] [--minutes]")
		return exitBadArgs
	}
	cwd, _ := os.Getwd()
	root, err := snapshot.RepoRoot(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema analyze: %v\n", err)
		return exitBadArgs
	}
	jobs, err := analyze.Repo(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cinema analyze: %v\n", err)
		return exitBadArgs
	}
	m := probeMachine()
	var mins map[string]float64
	if *withMinutes && !*asYAML {
		if mins, err = jobMinutes(30); err != nil {
			fmt.Fprintf(os.Stderr, "cinema analyze: minutes: %v\n", err)
		}
	}
	type localJob struct {
		analyze.Job
		Local   string   `json:"local"`
		Minutes *float64 `json:"median_billed_minutes,omitempty"`
	}
	local := make([]localJob, len(jobs))
	for i, j := range jobs {
		cat, _ := readiness(j, m)
		local[i] = localJob{Job: j, Local: cat}
		if v, ok := mins[analyzedKey(j)]; ok {
			local[i].Minutes = &v
		}
	}
	switch {
	case *asJSON:
		if err := envelope("analyze", map[string]any{"jobs": local, "machine": m}); err != nil {
			return exitInternal
		}
	case *asYAML:
		out, err := draft(jobs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cinema analyze: %v\n", err)
			return exitInternal
		}
		os.Stdout.Write(out)
	default:
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "VERDICT\tLOCAL\tWORKFLOW\tJOB\tWHY")
		counts := map[string]int{}
		for _, lj := range local {
			j := lj.Job
			counts[j.Verdict]++
			why := strings.Join(j.Reasons, "; ")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", j.Verdict, lj.Local, analyze.Printable(j.Workflow), analyze.Printable(jobName(j)), analyze.Printable(why))
		}
		tw.Flush()
		cm := make([]catMin, len(local))
		for i, l := range local {
			cm[i] = catMin{l.Local, l.Minutes}
		}
		printCapability(m, cm, mins != nil)
		fmt.Printf("\n%d task, %d review, %d skip. `cinema analyze --yaml` drafts tasks for the first two;\n"+
			"run each with `cinema run --commit HEAD <id>` and compare with CI before relying on it.\n",
			counts[analyze.Task], counts[analyze.Review], counts[analyze.Skip])
	}
	return 0
}

func jobName(j analyze.Job) string {
	if len(j.Combo) == 0 {
		return j.ID
	}
	var parts []string
	for k, v := range j.Combo {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return j.ID + " (" + strings.Join(parts, ", ") + ")"
}

// draft renders proposals as a tasks.yaml, with each job's open issues as
// comments. Values go through the YAML encoder, so scripts are quoted safely.
func draft(jobs []analyze.Job) ([]byte, error) { return draftFor(jobs, 4, 16) }

// draftFor drafts for a runner size: GitHub's standard Linux runner is 4 CPUs
// / 16 GB on public repos, 2 / 8 GB on private ones. A sandboxed run reports
// it and the CI runner measures its own, so it must be the repo's.
func draftFor(jobs []analyze.Job, cpus, memGB int) ([]byte, error) {
	m := func(kv ...*yaml.Node) *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Content: kv} }
	s := func(v string) *yaml.Node {
		n := &yaml.Node{Kind: yaml.ScalarNode, Value: v}
		if v == "" {
			n.Style = yaml.DoubleQuotedStyle // "" not an empty value (lab: eslint's yml/no-empty-mapping-value)
		}
		return n
	}
	flow := func(n *yaml.Node) *yaml.Node { n.Style = yaml.FlowStyle; return n }
	seq := func(vs ...string) *yaml.Node {
		n := &yaml.Node{Kind: yaml.SequenceNode}
		for _, v := range vs {
			n.Content = append(n.Content, s(v))
		}
		return n
	}
	tasks := m()
	seen := map[string]bool{}
	for _, j := range jobs {
		p := j.Proposal
		if p == nil || seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		script := s(p.Body)
		script.Style = yaml.LiteralStyle
		argv := seq("bash", "-eo", "pipefail", "-c") // Actions' default shell on Linux
		if len(p.Shell) > 0 {
			argv = seq(p.Shell...) // GitLab: bash where the image has it, else sh
		}
		argv.Content = append(argv.Content, script)
		t := m(s("argv"), argv, s("platform"), s("linux/amd64"))
		probes := &yaml.Node{Kind: yaml.SequenceNode}
		if p.Go != "" || p.Node != "" || len(p.Runtimes) > 0 {
			rt := m()
			if p.Go != "" {
				rt.Content = append(rt.Content, s("go"), s(p.Go))
				probes.Content = append(probes.Content, m(s("cmd"), flow(seq("go", "version"))))
			}
			if p.Node != "" {
				rt.Content = append(rt.Content, s("node"), s(p.Node))
				probes.Content = append(probes.Content, m(s("cmd"), flow(seq("node", "--version"))))
			}
			names := make([]string, 0, len(p.Runtimes))
			for n := range p.Runtimes {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				rt.Content = append(rt.Content, s(n), s(p.Runtimes[n]))
				if k := task.LookupRuntime(n); k != nil && len(k.Probe) > 0 {
					probes.Content = append(probes.Content, m(s("cmd"), flow(seq(k.Probe...))))
				}
			}
			t.Content = append(t.Content, s("runtime"), rt)
		}
		for _, pr := range p.Probes { // tools the job uses unpinned (the runner's python3)
			probes.Content = append(probes.Content, m(s("cmd"), flow(seq(pr...))))
		}
		if len(probes.Content) > 0 || p.Go != "" || p.Node != "" || len(p.Runtimes) > 0 {
			t.Content = append(t.Content, s("toolchain"), probes)
		}
		if p.Container != "" {
			t.Content = append(t.Content, s("container"), s(p.Container))
		}
		t.Content = append(t.Content, s("covers"), s(j.Workflow+"#"+j.ID), s("job_digest"), s(j.LegDigest()))
		if leg := legOf(j); leg != nil {
			l := m()
			for _, k := range j.MatrixKeys {
				l.Content = append(l.Content, s(k), s(leg[k]))
			}
			t.Content = append(t.Content, s("leg"), l)
		}
		if p.Exclusive {
			t.Content = append(t.Content, s("exclusive"), s("true"))
		}
		if len(p.Services) > 0 {
			var svcs yaml.Node
			if err := svcs.Encode(p.Services); err != nil {
				return nil, err
			}
			t.Content = append(t.Content, s("services"), &svcs)
		}
		if len(p.Outputs) > 0 {
			t.Content = append(t.Content, s("outputs"), flow(seq(p.Outputs...)))
			j.Notes = append(j.Notes, "outputs: are the paths the job uploads as artifacts; they're left out of the source fingerprint")
		}
		if len(p.Floating) > 0 {
			fl := m()
			keys := make([]string, 0, len(p.Floating))
			for k := range p.Floating {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fl.Content = append(fl.Content, s(k), s(p.Floating[k]))
			}
			t.Content = append(t.Content, s("floating"), fl)
		}
		if p.Event != "" {
			t.Content = append(t.Content, s("event"), s(p.Event))
			j.Notes = append(j.Notes, "the job reads the triggering event or ref: this task is evidence for "+p.Event+" runs only")
		}
		if strings.HasPrefix(strings.ToLower(j.RunsOn), "ubuntu-") {
			t.Content = append(t.Content, s("runs_on"), s(j.RunsOn))
		}
		cpus, memGB := cpus, memGB
		if p.CPUs > 0 && p.MemoryGB > 0 { // the job's runner, by its tags (GitLab)
			cpus, memGB = p.CPUs, p.MemoryGB
		}
		t.Content = append(t.Content, s("cpus"), s(fmt.Sprint(cpus)), s("memory"), s(fmt.Sprintf("%dg", memGB)), s("timeout"), s(fmt.Sprintf("%dm", cmp.Or(p.TimeoutMinutes, 60))),
			s("reuse"), m(s("allowed"), s("true"), s("max_age"), s("24h")))
		key := s(p.ID)
		key.HeadComment = analyze.Printable(fmt.Sprintf("%s: %s", j.Workflow, jobName(j)))
		for _, r := range j.Reasons {
			key.HeadComment += "\nREVIEW: " + analyze.Printable(r)
		}
		for _, n := range j.Notes {
			key.HeadComment += "\nNOTE: " + analyze.Printable(n)
		}
		tasks.Content = append(tasks.Content, key, t)
	}
	doc := m(s("schema_version"), s("1"), s("tasks"), tasks)
	doc.HeadComment = "Drafted by `cinema analyze` from .github/workflows. Not reviewed: every task must\n" +
		"pass `cinema run --commit HEAD <id>` and agree with CI before it counts for anything."
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{doc}}); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// legOf is the matrix leg j is, by the matrix's own keys; nil if j isn't a
// matrix job or a key's value isn't a plain one (object values are flattened).
func legOf(j analyze.Job) map[string]string {
	if len(j.MatrixKeys) == 0 {
		return nil
	}
	leg := map[string]string{}
	for _, k := range j.MatrixKeys {
		v, ok := j.Combo[k]
		if !ok {
			return nil
		}
		leg[k] = v
	}
	return leg
}
