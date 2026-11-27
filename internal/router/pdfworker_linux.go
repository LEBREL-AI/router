//go:build linux

package router

import "syscall"

// applyPDFWorkerLimits is what the kernel holds the reader to: the memory it may write to (its data, which is
// where a Go program's heap and stacks live; address space only reserved does not count), and its processor time.
func applyPDFWorkerLimits() {
	_ = syscall.Setrlimit(syscall.RLIMIT_DATA, &syscall.Rlimit{Cur: pdfWorkerMemory, Max: pdfWorkerMemory})
	_ = syscall.Setrlimit(syscall.RLIMIT_CPU, &syscall.Rlimit{Cur: pdfWorkerCPUSeconds, Max: pdfWorkerCPUSeconds + 2})
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
}
