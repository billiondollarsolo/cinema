package analyze

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

var (
	pinnedAction = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)((?:/[A-Za-z0-9_.-]+)*)@([0-9a-f]{40})$`)
	inputExpr    = regexp.MustCompile(`\$\{\{\s*inputs\.([A-Za-z0-9_-]+)\s*\}\}`)
	inputRef     = regexp.MustCompile(`\binputs\.([A-Za-z0-9_-]+)`)
)

// fetchAction reads a file from GitHub at a pinned commit (a var for tests).
var fetchAction = func(url string) ([]byte, error) {
	c := &http.Client{Timeout: 15 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if len(b) > 1<<20 {
		return nil, errors.New("action.yml over 1 MiB")
	}
	return b, err
}

// compositeSteps returns the steps of a composite action pinned to a commit,
// with its inputs (with: values, else defaults) substituted, so they can be
// translated like the job's own steps. Tag-pinned actions aren't inlined: their
// steps can change without a change to the workflow.
func compositeSteps(uses string, with map[string]any) ([]step, error) {
	m := pinnedAction.FindStringSubmatch(uses)
	if m == nil {
		return nil, errors.New("not pinned to a commit")
	}
	var b []byte
	var err error
	for _, f := range []string{"action.yml", "action.yaml"} {
		if b, err = fetchAction(fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s%s/%s", m[1], m[2], m[4], m[3], f)); err == nil {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	var a struct {
		Inputs map[string]struct {
			Default any `yaml:"default"`
		} `yaml:"inputs"`
		Runs struct {
			Using string `yaml:"using"`
			Steps []step `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err := yaml.Unmarshal(b, &a); err != nil {
		return nil, err
	}
	if a.Runs.Using != "composite" {
		return nil, errors.New("not a composite action")
	}
	val := map[string]string{}
	for k, in := range a.Inputs {
		val[k] = str(in.Default)
		if v, ok := with[k]; ok {
			val[k] = str(v)
		}
	}
	var bad error
	expand := func(s string) string {
		return inputExpr.ReplaceAllStringFunc(s, func(x string) string {
			k := inputExpr.FindStringSubmatch(x)[1]
			if _, ok := val[k]; !ok {
				bad = fmt.Errorf("unknown input %s", k)
			}
			return val[k]
		})
	}
	out := make([]step, 0, len(a.Runs.Steps))
	for _, s := range a.Runs.Steps {
		s.Run, s.Dir = expand(s.Run), expand(s.Dir)
		s.With, s.Env = expandMap(s.With, expand), expandMap(s.Env, expand)
		// In if: an input becomes a quoted literal the evaluator can compare.
		s.If = inputRef.ReplaceAllStringFunc(expand(s.If), func(x string) string {
			return "'" + strings.ReplaceAll(val[inputRef.FindStringSubmatch(x)[1]], "'", "''") + "'"
		})
		for _, t := range []string{s.Run, s.If, s.Dir} {
			if strings.Contains(t, "github.action_path") || inputRef.MatchString(t) {
				return nil, errors.New("refers to the action's own files or unresolved inputs")
			}
		}
		out = append(out, s)
	}
	return out, bad
}

func expandMap(m map[string]any, f func(string) string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = f(s)
		} else {
			out[k] = v
		}
	}
	return out
}
