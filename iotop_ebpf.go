//go:build linux && ebpf

package main

// eBPF-backed ioTracer. Built only with `-tags ebpf`; see iotop_stub.go for the
// default build and README.md for why it is opt-in.

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -tags ebpf -target bpfel -type io_key -type io_stats -cflags "-I/usr/include/x86_64-linux-gnu -O2 -g -Wall" zfsio ./bpf/zfsio.c

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

type ebpfTracer struct {
	coll  *ebpf.Collection
	links []link.Link
	stats *ebpf.Map
}

func newIOTracer() (ioTracer, error) {
	spec, err := loadZfsio()
	if err != nil {
		return nil, fmt.Errorf("reading embedded BPF object: %w", err)
	}

	// fexit programs resolve their attach target when they are *loaded*, not
	// when they are attached, so a kernel missing one of the four functions
	// would fail the whole collection. Drop those up front instead: losing
	// vfs_readv coverage is worth far more than losing all attribution.
	dropped := pruneMissingTargets(spec)
	if len(spec.Programs) == 0 {
		return nil, errors.New("kernel has none of the VFS functions zpulse traces (needs BTF and kernel >= 5.5)")
	}
	for _, name := range dropped {
		log.Printf("per-process I/O: kernel has no %s, that path will not be counted", name)
	}

	// Best effort, and deliberately not fatal. Kernels before 5.11 charge BPF
	// maps against RLIMIT_MEMLOCK; newer ones charge the cgroup instead, and
	// the probe used to tell the two apart itself needs the privileges we may
	// be missing. Failing here would mask the real problem behind a confusing
	// message about rlimits, so let the load below produce the error operators
	// can act on.
	_ = rlimit.RemoveMemlock()

	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		return nil, loadError(err)
	}

	t := &ebpfTracer{coll: coll, stats: coll.Maps["zfs_io"]}
	if t.stats == nil {
		coll.Close()
		return nil, errors.New("BPF object is missing the zfs_io map")
	}

	for name, prog := range coll.Programs {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			t.Close()
			return nil, fmt.Errorf("attaching %s: %w", name, err)
		}
		t.links = append(t.links, l)
	}

	return t, nil
}

// pruneMissingTargets removes program specs whose attach target does not exist
// in the running kernel's BTF, returning the names that were dropped.
func pruneMissingTargets(spec *ebpf.CollectionSpec) []string {
	kernel, err := btf.LoadKernelSpec()
	if err != nil {
		// No kernel BTF at all. Leave the spec alone and let the load fail
		// with its own, more specific error.
		return nil
	}

	var dropped []string
	for name, prog := range spec.Programs {
		var fn *btf.Func
		if err := kernel.TypeByName(prog.AttachTo, &fn); err != nil {
			dropped = append(dropped, prog.AttachTo)
			delete(spec.Programs, name)
		}
	}
	return dropped
}

// loadError adds the missing context to the failure operators actually hit:
// the process is not privileged enough to load a tracing program.
func loadError(err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("loading BPF programs requires CAP_BPF and CAP_PERFMON (or root): %w", err)
	}
	return fmt.Errorf("loading BPF programs: %w", err)
}

func (t *ebpfTracer) Sample() ([]ioSample, error) {
	var (
		key  zfsioIoKey
		val  zfsioIoStats
		out  []ioSample
		iter = t.stats.Iterate()
	)
	for iter.Next(&key, &val) {
		out = append(out, ioSample{
			ioKey:      ioKey{PID: key.Tgid, Dev: key.Dev},
			Comm:       commString(val.Comm[:]),
			ReadBytes:  val.ReadBytes,
			WriteBytes: val.WriteBytes,
			ReadOps:    val.ReadOps,
			WriteOps:   val.WriteOps,
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("reading BPF map: %w", err)
	}
	return out, nil
}

func (t *ebpfTracer) Forget(keys []ioKey) {
	for _, k := range keys {
		// A concurrent update can revive a key between the decision to
		// prune and the delete, and the key may already be gone. Neither
		// is worth reporting: the next sample re-baselines it.
		_ = t.stats.Delete(zfsioIoKey{Tgid: k.PID, Dev: k.Dev})
	}
}

func (t *ebpfTracer) Close() error {
	for _, l := range t.links {
		l.Close()
	}
	t.links = nil
	if t.coll != nil {
		t.coll.Close()
		t.coll = nil
	}
	return nil
}

// commString converts the fixed-size, NUL-padded comm from the kernel.
func commString(b []int8) string {
	buf := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}
