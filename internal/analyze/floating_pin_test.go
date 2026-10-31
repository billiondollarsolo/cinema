package analyze

import "testing"

// Lab (NSubstitute): dotnet-version "10.0.x\n9.0.x\n8.0.x" pinned 10.0.403 but
// recorded floating: 8.0.x, the last spec, so CI would compare the 10.0 pin
// with 8.0's newest. floating: is the spec the pin came from.
func TestFloatingIsThePinnedSpec(t *testing.T) {
	fakeDotnetIndex(t)
	j := dotnetJob(t, t.TempDir(), ownDir, `dotnet-version: "10.0.x\n9.0.x\n8.0.x", skip-lts-prepass: true`)
	if j.Proposal == nil || j.Proposal.Runtimes["dotnet"] != "10.0.403" || j.Proposal.Floating["dotnet"] != "10.0.x" {
		t.Fatalf("pin %v floating %v, want 10.0.403 from 10.0.x", j.Proposal.Runtimes, j.Proposal.Floating)
	}
}
