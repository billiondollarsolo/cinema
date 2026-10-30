package analyze

import "testing"

func TestEventDependentJobs(t *testing.T) {
	jobs, err := Workflow("ci.yml", []byte(`on: [push, pull_request]
jobs:
  plain:
    runs-on: ubuntu-latest
    steps: [{run: make test}]
  push-only-step:
    runs-on: ubuntu-latest
    steps:
      - run: make test
      - if: github.event_name == 'push'
        run: make integration
  reads-ref:
    runs-on: ubuntu-latest
    steps: [{run: 'echo "$GITHUB_REF"'}]
`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"plain": "", "push-only-step": "pull_request", "reads-ref": "pull_request"}
	for _, j := range jobs {
		if j.Proposal == nil || j.Proposal.Event != want[j.ID] {
			t.Errorf("%s: event %q, want %q", j.ID, j.Proposal.Event, want[j.ID])
		}
	}
}
