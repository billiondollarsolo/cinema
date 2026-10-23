package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"

	"github.com/billiondollarsolo/cinema/internal/analyze"
	"github.com/billiondollarsolo/cinema/internal/snapshot"
)

// cmdAnalyze proposes tasks from the repo's GitHub Actions workflows. Advisory
// only: it prints, never writes tasks.yaml. A proposal is checked by running it
// (`cinema run`) and comparing with CI, not by reading it.
func cmdAnalyze(args []string) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	asYAML := fs.Bool("yaml", false, "print proposed tasks as a tasks.yaml draft")
	asJSON := fs.Bool("json", false, "machine-readable result")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: cinema analyze [--yaml | --json]")
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
	switch {
	case *asJSON:
		if err := envelope("analyze", map[string]any{"jobs": jobs}); err != nil {
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
		fmt.Fprintln(tw, "VERDICT\tWORKFLOW\tJOB\tWHY")
		counts := map[string]int{}
		for _, j := range jobs {
			counts[j.Verdict]++
			why := strings.Join(j.Reasons, "; ")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", j.Verdict, analyze.Printable(j.Workflow), analyze.Printable(jobName(j)), analyze.Printable(why))
		}
		tw.Flush()
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
func draft(jobs []analyze.Job) ([]byte, error) {
	m := func(kv ...*yaml.Node) *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Content: kv} }
	s := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }
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
		argv.Content = append(argv.Content, script)
		t := m(s("argv"), argv, s("platform"), s("linux/amd64"))
		if p.Go != "" || p.Node != "" {
			rt, probes := m(), &yaml.Node{Kind: yaml.SequenceNode}
			if p.Go != "" {
				rt.Content = append(rt.Content, s("go"), s(p.Go))
				probes.Content = append(probes.Content, m(s("cmd"), flow(seq("go", "version"))))
			}
			if p.Node != "" {
				rt.Content = append(rt.Content, s("node"), s(p.Node))
				probes.Content = append(probes.Content, m(s("cmd"), flow(seq("node", "--version"))))
			}
			t.Content = append(t.Content, s("runtime"), rt, s("toolchain"), probes)
		}
		if p.Container != "" {
			t.Content = append(t.Content, s("container"), s(p.Container))
		}
		if len(p.Outputs) > 0 {
			t.Content = append(t.Content, s("outputs"), flow(seq(p.Outputs...)))
			j.Notes = append(j.Notes, "outputs: are the paths the job uploads as artifacts; they're left out of the source fingerprint")
		}
		// GitHub's standard Linux runner has 4 vCPUs (public repos; 2 on private ones).
		t.Content = append(t.Content, s("cpus"), s("4"), s("timeout"), s("60m"),
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
