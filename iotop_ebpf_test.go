//go:build linux && ebpf

package main

import (
	"os"
	"testing"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
)

// wantTargets are the kernel functions bpf/zfsio.c hooks. vfs_read and
// vfs_write carry the bulk of the traffic; the iovec pair matters for callers
// like qemu that never use the plain forms.
var wantTargets = map[string]string{
	"zfs_vfs_read":   "vfs_read",
	"zfs_vfs_write":  "vfs_write",
	"zfs_vfs_readv":  "vfs_readv",
	"zfs_vfs_writev": "vfs_writev",
}

func TestEmbeddedObjectShape(t *testing.T) {
	spec, err := loadZfsio()
	if err != nil {
		t.Fatalf("loadZfsio: %v", err)
	}

	if len(spec.Programs) != len(wantTargets) {
		t.Errorf("got %d programs, want %d", len(spec.Programs), len(wantTargets))
	}
	for name, target := range wantTargets {
		ps, ok := spec.Programs[name]
		if !ok {
			t.Errorf("program %q missing from object", name)
			continue
		}
		if ps.AttachTo != target {
			t.Errorf("%s attaches to %q, want %q", name, ps.AttachTo, target)
		}
		if ps.Type != ebpf.Tracing || ps.AttachType != ebpf.AttachTraceFExit {
			t.Errorf("%s is %v/%v, want Tracing/AttachTraceFExit", name, ps.Type, ps.AttachType)
		}
	}

	m, ok := spec.Maps["zfs_io"]
	if !ok {
		t.Fatal("zfs_io map missing from object")
	}
	if m.Type != ebpf.Hash {
		t.Errorf("zfs_io is %v, want Hash", m.Type)
	}

	// If these drift apart, Iterate silently decodes garbage into the Go
	// structs rather than failing, so assert the layouts match exactly.
	if got, want := m.KeySize, uint32(unsafe.Sizeof(zfsioIoKey{})); got != want {
		t.Errorf("map key is %d bytes, Go struct is %d", got, want)
	}
	if got, want := m.ValueSize, uint32(unsafe.Sizeof(zfsioIoStats{})); got != want {
		t.Errorf("map value is %d bytes, Go struct is %d", got, want)
	}
}

func TestPruneMissingTargetsKeepsWhatTheKernelHas(t *testing.T) {
	if _, err := btf.LoadKernelSpec(); err != nil {
		t.Skipf("no kernel BTF on this host: %v", err)
	}

	spec, err := loadZfsio()
	if err != nil {
		t.Fatal(err)
	}

	// A target no kernel will ever have, alongside the real ones.
	spec.Programs["bogus"] = &ebpf.ProgramSpec{
		Name:       "bogus",
		Type:       ebpf.Tracing,
		AttachType: ebpf.AttachTraceFExit,
		AttachTo:   "zpulse_not_a_real_kernel_function",
	}

	dropped := pruneMissingTargets(spec)

	if _, still := spec.Programs["bogus"]; still {
		t.Error("pruneMissingTargets kept a program with no kernel target")
	}
	if len(dropped) == 0 {
		t.Error("pruneMissingTargets reported nothing dropped")
	}

	// vfs_read and vfs_write have been in the kernel for decades; if they are
	// gone the hook choice itself is wrong, not just this test.
	for _, name := range []string{"zfs_vfs_read", "zfs_vfs_write"} {
		if _, ok := spec.Programs[name]; !ok {
			t.Errorf("%s was pruned, but its target should exist on any supported kernel", name)
		}
	}
}

// TestLoadAndAttach exercises the verifier and the attach path, which no amount
// of static checking substitutes for. It needs root, so it is skipped in normal
// runs: `sudo -E go test -tags ebpf -run TestLoadAndAttach ./...`
func TestLoadAndAttach(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root (CAP_BPF + CAP_PERFMON) to load tracing programs")
	}

	tracer, err := newIOTracer()
	if err != nil {
		t.Fatalf("newIOTracer: %v", err)
	}
	defer tracer.Close()

	// The map starts empty and reading it must work even so.
	samples, err := tracer.Sample()
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	t.Logf("attached; %d entries in the map", len(samples))

	// Deleting keys that were never there must not error out.
	tracer.Forget([]ioKey{{PID: 1 << 30, Dev: 0}})
}
