//go:build !linux

package record

import "os/exec"

// ponytail: no CPU affinity API on macOS; cpus: is a no-op there.
func start(cmd *exec.Cmd, _ []int) error { return cmd.Start() }
