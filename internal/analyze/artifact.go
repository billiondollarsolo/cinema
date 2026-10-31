package analyze

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// A job that downloads an artifact another job of its workflow builds (lab:
// traefik's 19 cross-builds need the web UI, prettier's tests need dist/) can
// still be a task: it builds the artifact itself, the way the producing job
// does, in a fresh copy of the commit (that job's own runner), and takes only
// the artifact from it. CI builds it once and shares it; locally it's built
// per task: slower, same result.

// artifactSteps returns what replaces a download-artifact step: a script that
// copies the pristine tree before any of the job's own steps run (pre), and
// the producing job's steps, run in that copy, then the artifact copied into
// place (steps). why says why the download can't be reproduced.
func artifactSteps(root string, w *workflow, consumer *job, with map[string]any, n int) (pre string, steps []step, producer, why string) {
	name := str(with["name"])
	for _, k := range []string{"pattern", "merge-multiple", "run-id", "github-token", "repository"} {
		if with[k] != nil {
			return "", nil, "", "needs artifacts from another job (download-artifact with " + k + ")"
		}
	}
	if name == "" || strings.Contains(name, "${{") {
		return "", nil, "", "needs artifacts from another job (download-artifact without a literal name)"
	}
	var needs []string
	switch consumer.Needs.Kind {
	case yaml.ScalarNode:
		needs = []string{consumer.Needs.Value}
	case yaml.SequenceNode:
		for _, c := range consumer.Needs.Content {
			needs = append(needs, c.Value)
		}
	}
	for _, id := range needs {
		pj, penv := w.Jobs[id], map[string]any(nil)
		if pj == nil {
			continue
		}
		if local, ok := strings.CutPrefix(pj.Uses, "./.github/workflows/"); ok && root != "" && !strings.Contains(local, "/") {
			pj, penv = reusableJob(root, ".github/workflows/"+local)
			if pj == nil {
				continue
			}
		} else {
			penv = w.Env
		}
		up := -1
		for i, s := range pj.Steps {
			if strings.HasPrefix(s.Uses, "actions/upload-artifact@") && str(s.With["name"]) == name {
				up = i
				break
			}
		}
		if up < 0 {
			continue
		}
		producer = id
		switch {
		case pj.Uses != "", pj.Strategy.Matrix.Kind != 0, !pj.Container.IsZero(), len(pj.Services) > 0, pj.If != "", !pj.Needs.IsZero():
			return "", nil, id, "needs artifact " + name + " from job " + id + ", which uses a matrix, container, services, needs or if: cinema rebuilds only plain producers"
		case !strings.Contains(scalarOrList(&pj.RunsOn), "ubuntu") || strings.Contains(scalarOrList(&pj.RunsOn), "${{"):
			return "", nil, id, "needs artifact " + name + " from job " + id + ", which runs on " + scalarOrList(&pj.RunsOn)
		}
		path := strings.TrimSpace(str(pj.Steps[up].With["path"]))
		if path == "" || strings.ContainsAny(path, "\n*?[!$") || !filepath.IsLocal(path) {
			return "", nil, id, fmt.Sprintf("needs artifact %s from job %s, uploaded from %q: cinema copies one literal path", name, id, path)
		}
		// upload-artifact stores paths relative to their common root (a
		// directory's contents, a file's name), so the download lands where it
		// was built only if its path is that root. Which one it is is checked
		// when the task runs.
		upPath, dl := filepath.Clean(path), filepath.Clean(cmp.Or(str(with["path"]), "."))
		p := fmt.Sprintf(`"${TMPDIR:-/tmp}/cinema-artifact-$$-%d"`, n)
		var place string
		switch {
		case dl == upPath:
			place = fmt.Sprintf("test -d %[1]s/%[2]s || { echo 'cinema: artifact %[3]s: %[2]s is not a directory: its layout would differ from CI' >&2; exit 1; }\nmkdir -p %[4]s && cp -a %[1]s/%[2]s/. %[4]s/", p, shellQuote(upPath), name, shellQuote(dl))
		case dl == filepath.Dir(upPath):
			place = fmt.Sprintf("test -f %[1]s/%[2]s || { echo 'cinema: artifact %[3]s: %[2]s is not a file: its layout would differ from CI' >&2; exit 1; }\nmkdir -p %[4]s && cp -a %[1]s/%[2]s %[4]s/", p, shellQuote(upPath), name, shellQuote(dl))
		default:
			return "", nil, id, fmt.Sprintf("needs artifact %s from job %s, downloaded to %s but uploaded from %s: cinema can't place it", name, id, dl, upPath)
		}
		// The producer's runner has its own env, not this job's: unset the
		// consumer's job/workflow env (exported for the whole task) in its steps.
		var unset []string
		for _, m := range []map[string]any{w.Env, consumer.Env} {
			for _, k := range sortedKeys(m) {
				if _, ok := penv[k]; !ok && pj.Env[k] == nil {
					unset = append(unset, k)
				}
			}
		}
		// Shared between tasks (cinema run sets CINEMA_ARTIFACTS for clean
		// checkouts): built once per commit, producer job and Go/Node version,
		// not once per task (lab: traefik's 19 legs each rebuilt the web UI).
		// A hit skips the producer's steps; a failed build is never stored.
		def, _ := json.Marshal([]any{id, name, upPath, penv, pj.Env, pj.Defaults, unset, pj.Steps[:up]})
		sum := sha256.Sum256(def)
		key := fmt.Sprintf(`c=$(git rev-parse --verify HEAD 2>/dev/null) && K="$CINEMA_ARTIFACTS/%x-$c-$( { node --version; go version; } 2>&1 | sha256sum | cut -c1-16)"`, sum[:8])
		cached := `[ -n "${CINEMA_ARTIFACTS:-}" ] && command -v sha256sum >/dev/null && ` + key
		at := "cd " + p + "\n  [ -e .cinema-hit ] && exit 0"
		if len(unset) > 0 {
			at += "\n  unset " + strings.Join(unset, " ")
		}
		for _, s := range pj.Steps[:up] {
			if strings.HasPrefix(s.Uses, "actions/upload-artifact@") {
				continue // other uploads: not this job's outputs
			}
			s.Env = merged(penv, pj.Env, s.Env)
			if s.Dir == "" {
				s.Dir = cmp.Or(pj.Defaults.Run.WorkingDirectory, ".")
			}
			s.at = at
			steps = append(steps, s)
		}
		// Store before placing: one writer per key (mkdir as the lock), into a
		// temp dir renamed into place, and never failing the task.
		store := fmt.Sprintf(`if [ ! -e %[1]s/.cinema-hit ] && %[2]s; then
  ( set +e; mkdir -p "$CINEMA_ARTIFACTS" && mkdir "$K.lock" 2>/dev/null || exit 0
    [ -e "$K" ] || { rm -rf "$K.tmp" && mkdir "$K.tmp" && cp -a %[1]s/%[3]s "$K.tmp/a" && mv "$K.tmp" "$K"; }
    rm -rf "$K.tmp" "$K.lock" ) || true
fi
`, p, cached, shellQuote(upPath))
		steps = append(steps, step{Run: store + place + "\nrm -rf " + p, Dir: "."})
		// mkdir, not mkdir -p: atomic, and fails on anything already there (a
		// planted dir or symlink, when TMPDIR is the shared /tmp).
		pre = fmt.Sprintf(`(
  mkdir -m 700 %[1]s
  if %[2]s && [ -e "$K/a" ]; then
    touch "$K" && mkdir -p %[1]s/%[3]s && cp -a "$K/a" %[1]s/%[4]s && touch %[1]s/.cinema-hit
  else
    cp -a . %[1]s/
  fi
)`, p, cached, shellQuote(filepath.Dir(upPath)), shellQuote(upPath))
		return pre, steps, id, ""
	}
	return "", nil, "", "needs artifacts from another job (download-artifact: no job it needs uploads " + name + ")"
}

// reusableJob is the single job of a local reusable workflow, with its env.
func reusableJob(root, rel string) (*job, map[string]any) {
	b, err := readRepo(root, rel)
	if err != nil {
		return nil, nil
	}
	var w workflow
	if yaml.Unmarshal(b, &w) != nil || len(w.Jobs) != 1 {
		return nil, nil
	}
	for _, j := range w.Jobs {
		return j, w.Env
	}
	return nil, nil
}

func merged(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
