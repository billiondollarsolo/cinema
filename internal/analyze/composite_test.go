package analyze

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// Tests never reach the network: the composite test installs its own action.
func TestMain(m *testing.M) {
	fetchAction = func(string) ([]byte, error) { return nil, errors.New("offline") }
	os.Exit(m.Run())
}

const sha = "55bd31fe92710532260eb57c554f2697363285c5"

func TestCompositeInlined(t *testing.T) {
	fetchAction = func(url string) ([]byte, error) {
		if !strings.Contains(url, "/prometheus/promci-setup/"+sha+"/action.yml") {
			return nil, errors.New("404")
		}
		return []byte(`inputs:
  enable_go: {default: "true"}
  enable_docker_multibuild: {default: "false"}
  cache_key_suffix: {default: ""}
runs:
  using: composite
  steps:
    - uses: actions/cache@v4
      if: inputs.enable_go == 'true'
      with: {key: "go-${{ inputs.cache_key_suffix }}"}
    - run: echo "GOMEMLIMIT=1GiB" >> $GITHUB_ENV
      shell: bash
      if: inputs.enable_go == 'true'
    - run: make promu
      shell: bash
      if: ${{ inputs.enable_go == 'true' }}
    - uses: docker/setup-buildx-action@v3
      if: inputs.enable_docker_multibuild == 'true'
`), nil
	}
	defer func() { fetchAction = func(string) ([]byte, error) { return nil, errors.New("offline") } }()
	src := `on: pull_request
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: prometheus/promci-setup@` + sha + `
        with: {cache_key_suffix: more}
      - run: go test ./...
  tagged:
    runs-on: ubuntu-latest
    steps:
      - uses: prometheus/promci-setup@v0.2.2
      - run: go test ./...
`
	jobs, err := Workflow("ci.yml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		switch j.ID {
		case "test":
			if j.Verdict != Task || !strings.Contains(j.Proposal.Body, "make promu") || !strings.Contains(j.Proposal.Body, "GOMEMLIMIT") || strings.Contains(j.Proposal.Body, "buildx") {
				t.Errorf("test: %s %q\n%s", j.Verdict, j.Reasons, j.Proposal.Body)
			}
		case "tagged":
			if j.Verdict != Review {
				t.Errorf("tag-pinned action inlined: %s", j.Verdict)
			}
		}
	}
}
