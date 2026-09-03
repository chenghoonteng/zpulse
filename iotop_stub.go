//go:build !linux || !ebpf

package main

import "errors"

// newIOTracer reports that this binary has no counter source compiled in.
//
// Per-process attribution is the one part of zpulse that needs a kernel-side
// component, elevated privileges, and a non-stdlib dependency, so it is behind
// a build tag rather than on by default. zpulse shows this message in place of
// the process table; everything else works as usual.
func newIOTracer() (ioTracer, error) {
	return nil, errors.New("not compiled in: rebuild with `go build -tags ebpf` on Linux")
}
