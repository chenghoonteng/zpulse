package main

// Per-process I/O attribution.
//
// Everything zpulse reads from `zpool`, `zfs` and the ZFS kstats is aggregate:
// it can say a dataset is busy but never which process made it busy. This file
// holds the platform-independent half of the answer -- turning cumulative
// counters into rates, naming processes, and mapping device ids back to dataset
// names. The counters themselves come from an ioTracer, which on Linux is an
// eBPF program (see bpf/zfsio.c) and elsewhere is a stub.

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcIO is one process's traffic against one dataset, as rates over the last
// sampling interval.
type ProcIO struct {
	PID       int
	Command   string
	Dataset   string
	ReadBps   uint64 // bytes per second
	WriteBps  uint64
	ReadIOPS  uint64
	WriteIOPS uint64
}

// ioKey identifies one process's traffic against one dataset.
type ioKey struct {
	PID uint32
	Dev uint32 // superblock device id, matched against /proc/self/mountinfo
}

// ioSample is the cumulative counter set for one ioKey, read straight out of
// the tracer. Counters only ever increase; rates are differences between two
// samples.
type ioSample struct {
	ioKey
	Comm       string
	ReadBytes  uint64
	WriteBytes uint64
	ReadOps    uint64
	WriteOps   uint64
}

// ioTracer is the platform-specific counter source. The eBPF implementation
// lives in iotop_ebpf.go, behind the `ebpf` build tag.
type ioTracer interface {
	// Sample returns the current cumulative counters for every tracked pair.
	Sample() ([]ioSample, error)
	// Forget drops counters for pairs that will never be updated again,
	// keeping the tracer's table from filling up with exited processes.
	Forget(keys []ioKey)
	// Close detaches the tracer.
	Close() error
}

// ioAccounting converts a tracer's cumulative counters into per-interval rates.
// It is not safe for concurrent use; ioMonitor owns the only instance.
type ioAccounting struct {
	tracer ioTracer
	mounts mountTable
	prev   map[ioKey]ioSample
	prevAt time.Time
	now    func() time.Time // swapped out in tests
}

func newIOAccounting(t ioTracer) *ioAccounting {
	return &ioAccounting{tracer: t, now: time.Now}
}

func (a *ioAccounting) Close() error { return a.tracer.Close() }

// Rates samples the tracer and returns the busiest limit pairs by combined
// throughput. The first call only establishes a baseline and returns nothing:
// the counters at that point cover an unknown window reaching back to whenever
// each process started, so reporting them as a rate would invent a spike.
func (a *ioAccounting) Rates(limit int) ([]ProcIO, error) {
	samples, err := a.tracer.Sample()
	if err != nil {
		return nil, err
	}
	now := a.now()

	cur := make(map[ioKey]ioSample, len(samples))
	for _, s := range samples {
		cur[s.ioKey] = s
	}

	prev, prevAt := a.prev, a.prevAt
	a.prev, a.prevAt = cur, now

	if prev == nil {
		return nil, nil
	}
	elapsed := now.Sub(prevAt).Seconds()
	if elapsed <= 0 {
		return nil, nil
	}

	var (
		rows []ProcIO
		idle []ioKey // candidates for pruning: no I/O since the last sample
	)
	for key, s := range cur {
		p, seen := prev[key]
		if !seen {
			// New pair. Its counters start from zero at first sight, but
			// the process may have been running long before that, so the
			// window is still unknown. Baseline it and report next round.
			continue
		}

		rd, rdOK := delta(s.ReadBytes, p.ReadBytes)
		wr, wrOK := delta(s.WriteBytes, p.WriteBytes)
		rops, _ := delta(s.ReadOps, p.ReadOps)
		wops, _ := delta(s.WriteOps, p.WriteOps)
		if !rdOK || !wrOK {
			// Counters went backwards, which means this key was reused by
			// a new process after a PID wrap. Re-baseline rather than
			// report a nonsense rate.
			continue
		}

		if rd == 0 && wr == 0 {
			idle = append(idle, key)
			continue
		}

		rows = append(rows, ProcIO{
			PID:       int(key.PID),
			Command:   s.Comm,
			Dataset:   a.mounts.dataset(key.Dev),
			ReadBps:   perSecond(rd, elapsed),
			WriteBps:  perSecond(wr, elapsed),
			ReadIOPS:  perSecond(rops, elapsed),
			WriteIOPS: perSecond(wops, elapsed),
		})
	}

	a.prune(idle)

	// Busiest first, with the PID as a tiebreak so equal rows do not shuffle
	// between refreshes.
	sort.Slice(rows, func(i, j int) bool {
		bi, bj := rows[i].ReadBps+rows[i].WriteBps, rows[j].ReadBps+rows[j].WriteBps
		if bi != bj {
			return bi > bj
		}
		return rows[i].PID < rows[j].PID
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	// Only the rows that survived the cut are worth a /proc lookup.
	for i := range rows {
		rows[i].Command = commandName(rows[i].PID, rows[i].Command)
	}
	return rows, nil
}

// prune drops tracer entries for processes that have exited. Only keys that
// were idle for a full interval are considered, so a process's last burst of
// I/O is always reported before its entry goes away.
func (a *ioAccounting) prune(idle []ioKey) {
	var dead []ioKey
	for _, k := range idle {
		if !processExists(int(k.PID)) {
			dead = append(dead, k)
			delete(a.prev, k)
		}
	}
	if len(dead) > 0 {
		a.tracer.Forget(dead)
	}
}

// delta returns cur-prev, reporting false if the counter went backwards.
func delta(cur, prev uint64) (uint64, bool) {
	if cur < prev {
		return 0, false
	}
	return cur - prev, true
}

func perSecond(n uint64, seconds float64) uint64 {
	return uint64(float64(n)/seconds + 0.5)
}

func processExists(pid int) bool {
	_, err := os.Stat("/proc/" + strconv.Itoa(pid))
	return err == nil
}

// commandName gives a friendlier name than the 15-character comm the tracer
// captured, by reading the executable out of /proc. The process may well have
// exited since it was sampled, in which case comm is all we will ever have.
func commandName(pid int, comm string) string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(raw) == 0 {
		return comm
	}
	// cmdline is NUL-separated; the first entry is argv[0].
	argv0, _, _ := strings.Cut(string(raw), "\x00")
	if argv0 == "" {
		return comm
	}
	return filepath.Base(argv0)
}

// mountTable maps a superblock device id to the ZFS dataset mounted on it.
//
// /proc/self/mountinfo reports each mount's device as the major:minor of the
// same dev_t the kernel keeps in super_block.s_dev, which is exactly what the
// tracer records. So this is a direct lookup with no path matching involved,
// and it keeps dataset naming out of the BPF program entirely.
type mountTable struct {
	byDev  map[uint32]string
	loaded time.Time
}

// mountReloadInterval bounds how often an unknown device triggers a re-read of
// mountinfo, so a device that genuinely has no ZFS mount cannot make every
// sample re-parse the file.
const mountReloadInterval = 2 * time.Second

func (m *mountTable) dataset(dev uint32) string {
	if name, ok := m.byDev[dev]; ok {
		return name
	}
	if time.Since(m.loaded) < mountReloadInterval {
		return unknownDataset(dev)
	}
	m.reload()
	if name, ok := m.byDev[dev]; ok {
		return name
	}
	return unknownDataset(dev)
}

func (m *mountTable) reload() {
	m.loaded = time.Now()
	if byDev, err := parseMountinfo("/proc/self/mountinfo"); err == nil {
		m.byDev = byDev
	}
}

// unknownDataset labels a device that has no ZFS mount in this namespace --
// most often because the mount lives in a container's namespace, not ours.
func unknownDataset(dev uint32) string {
	return fmt.Sprintf("dev %d:%d", dev>>20, dev&0xfffff)
}

// parseMountinfo returns the ZFS mounts keyed by device id.
//
// Lines look like:
//
//	36 25 0:35 / /tank rw,noatime shared:1 - zfs tank rw,xattr,noacl
//
// Field 2 is the device, and the fields after the "-" separator are the
// filesystem type and the mount source -- for ZFS, the dataset name. The
// optional fields before the separator are variable in number, which is why
// the separator has to be searched for rather than indexed.
func parseMountinfo(path string) (map[uint32]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	byDev := make(map[uint32]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			continue
		}

		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) {
			continue
		}
		if fields[sep+1] != "zfs" {
			continue
		}

		dev, ok := parseDev(fields[2])
		if !ok {
			continue
		}
		// A dataset can be mounted more than once; either name is correct,
		// so the first one wins and stays stable across reloads.
		if _, seen := byDev[dev]; !seen {
			byDev[dev] = fields[sep+2]
		}
	}
	return byDev, scanner.Err()
}

// parseDev converts a "major:minor" pair into the kernel's packed dev_t.
func parseDev(s string) (uint32, bool) {
	majStr, minStr, ok := strings.Cut(s, ":")
	if !ok {
		return 0, false
	}
	major, err := strconv.ParseUint(majStr, 10, 32)
	if err != nil {
		return 0, false
	}
	minor, err := strconv.ParseUint(minStr, 10, 32)
	if err != nil {
		return 0, false
	}
	// MKDEV: 20 bits of minor, the rest major.
	return uint32(major<<20 | (minor & 0xfffff)), true
}

// ioMonitor samples per-process I/O on a fixed cadence and caches the latest
// result for `zpulse watch` to render.
//
// The sampling has to be decoupled from the redraw. Rates come from the
// difference between two reads of the counters, so a redraw that sampled for
// itself would report whatever happened since the last frame -- and would
// report nothing at all on any frame drawn early, such as the first one.
type ioMonitor struct {
	mu      sync.RWMutex
	latest  []ProcIO
	err     string
	ready   bool
	stop    chan struct{}
	done    chan struct{} // closed once the sampling goroutine has returned
	acct    *ioAccounting
	stopped sync.Once
}

// startIOMonitor begins sampling, or returns a monitor that explains why it
// could not. A failure here is never fatal: per-process attribution needs
// privileges and a kernel that the rest of zpulse does not, so the output
// degrades to its other sections rather than refusing to start.
func startIOMonitor(cfg Config) *ioMonitor {
	m := &ioMonitor{stop: make(chan struct{}), done: make(chan struct{})}

	if cfg.TopProcesses <= 0 {
		m.err = "disabled in config (top_processes = 0)"
		close(m.done)
		return m
	}

	tracer, err := newIOTracer()
	if err != nil {
		m.err = err.Error()
		log.Printf("per-process I/O unavailable: %v", err)
		close(m.done)
		return m
	}

	m.acct = newIOAccounting(tracer)
	interval := time.Duration(cfg.RefreshInterval) * time.Second

	go func() {
		defer close(m.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.stop:
				return
			case <-ticker.C:
				rows, err := m.acct.Rates(cfg.TopProcesses)
				m.mu.Lock()
				if err != nil {
					m.err = err.Error()
				} else {
					m.latest, m.err, m.ready = rows, "", true
				}
				m.mu.Unlock()
			}
		}
	}()

	return m
}

// Snapshot returns the most recent sample, an error to show in its place, and
// whether a full interval has elapsed yet.
func (m *ioMonitor) Snapshot() (rows []ProcIO, errText string, ready bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.latest, m.err, m.ready
}

// Close stops sampling and detaches the tracer. It waits for an in-flight
// sample to finish first: tearing the tracer down underneath a running Rates
// call would have it read from an already-closed map.
func (m *ioMonitor) Close() {
	m.stopped.Do(func() {
		close(m.stop)
		<-m.done
		if m.acct != nil {
			m.acct.Close()
		}
	})
}
