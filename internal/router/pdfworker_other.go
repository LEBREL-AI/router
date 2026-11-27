//go:build !linux

package router

// applyPDFWorkerLimits: the router runs on Linux, where the kernel holds the reader to its limits; elsewhere (a
// developer's machine) the reader has its stack ceiling, its soft memory limit and the deadline.
func applyPDFWorkerLimits() {}
