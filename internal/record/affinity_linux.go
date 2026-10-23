package record

import (
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// start starts cmd on only cpus. A forked child inherits the CPU mask of the
// thread that forks it, so pin this (locked) thread, fork, and restore it. The
// mask is what nproc, Go's NumCPU and Node's availableParallelism report.
func start(cmd *exec.Cmd, cpus []int) error {
	if len(cpus) == 0 {
		return cmd.Start()
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var old, set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &old); err != nil {
		return err
	}
	for _, c := range cpus {
		set.Set(c)
	}
	if err := unix.SchedSetaffinity(0, &set); err != nil {
		return err
	}
	defer unix.SchedSetaffinity(0, &old)
	return cmd.Start()
}
