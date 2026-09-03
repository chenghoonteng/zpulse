package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeTracer replays a fixed list of samples, one per Sample() call, and
// records what the accounting asked it to forget.
type fakeTracer struct {
	rounds [][]ioSample
	call   int
	forgot []ioKey
}

func (f *fakeTracer) Sample() ([]ioSample, error) {
	if f.call >= len(f.rounds) {
		return nil, nil
	}
	s := f.rounds[f.call]
	f.call++
	return s, nil
}

func (f *fakeTracer) Forget(keys []ioKey) { f.forgot = append(f.forgot, keys...) }
func (f *fakeTracer) Close() error        { return nil }

// newTestAccounting wires an accounting with a controllable clock that advances
// by the given step on every sample.
func newTestAccounting(t *testing.T, step time.Duration, rounds ...[]ioSample) (*ioAccounting, *fakeTracer) {
	t.Helper()
	tracer := &fakeTracer{rounds: rounds}
	a := newIOAccounting(tracer)
	clock := time.Unix(0, 0)
	a.now = func() time.Time {
		clock = clock.Add(step)
		return clock
	}
	// Pin the dataset names so the tests do not depend on the host's mounts.
	a.mounts = mountTable{
		byDev:  map[uint32]string{35: "tank/db", 36: "tank/media"},
		loaded: time.Now().Add(time.Hour), // never reload
	}
	return a, tracer
}

func sample(pid, dev uint32, comm string, rb, wb, ro, wo uint64) ioSample {
	return ioSample{
		ioKey:      ioKey{PID: pid, Dev: dev},
		Comm:       comm,
		ReadBytes:  rb,
		WriteBytes: wb,
		ReadOps:    ro,
		WriteOps:   wo,
	}
}

func TestRatesFirstSampleIsBaselineOnly(t *testing.T) {
	// A process that has already read 100 MB before zpulse started must not be
	// reported as having read 100 MB during the first interval.
	a, _ := newTestAccounting(t, 10*time.Second,
		[]ioSample{sample(42, 35, "postgres", 100<<20, 0, 500, 0)},
	)

	rows, err := a.Rates(10)
	if err != nil {
		t.Fatalf("Rates: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("first sample produced %d rows, want 0 (baseline only): %+v", len(rows), rows)
	}
}

func TestRatesComputesPerSecondDeltas(t *testing.T) {
	pid := uint32(os.Getpid()) // alive, so pruning cannot interfere

	a, _ := newTestAccounting(t, 10*time.Second,
		[]ioSample{sample(pid, 35, "postgres", 1000, 2000, 10, 20)},
		[]ioSample{sample(pid, 35, "postgres", 1000+50<<20, 2000+10<<20, 10+800, 20+160)},
	)

	if _, err := a.Rates(10); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	rows, err := a.Rates(10)
	if err != nil {
		t.Fatalf("Rates: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}

	got := rows[0]
	if want := uint64(50 << 20 / 10); got.ReadBps != want {
		t.Errorf("ReadBps = %d, want %d", got.ReadBps, want)
	}
	if want := uint64(10 << 20 / 10); got.WriteBps != want {
		t.Errorf("WriteBps = %d, want %d", got.WriteBps, want)
	}
	if got.ReadIOPS != 80 {
		t.Errorf("ReadIOPS = %d, want 80", got.ReadIOPS)
	}
	if got.WriteIOPS != 16 {
		t.Errorf("WriteIOPS = %d, want 16", got.WriteIOPS)
	}
	if got.Dataset != "tank/db" {
		t.Errorf("Dataset = %q, want %q", got.Dataset, "tank/db")
	}
	if got.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", got.PID, os.Getpid())
	}
}

func TestRatesSortsByThroughputAndLimits(t *testing.T) {
	pid := uint32(os.Getpid())
	base := []ioSample{
		sample(pid, 35, "slow", 0, 0, 0, 0),
		sample(pid+1, 35, "fast", 0, 0, 0, 0),
		sample(pid+2, 36, "middle", 0, 0, 0, 0),
	}
	next := []ioSample{
		sample(pid, 35, "slow", 1<<20, 0, 1, 0),
		sample(pid+1, 35, "fast", 0, 100<<20, 0, 1), // writes count too
		sample(pid+2, 36, "middle", 5<<20, 5<<20, 1, 1),
	}

	a, _ := newTestAccounting(t, time.Second, base, next)
	if _, err := a.Rates(10); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Rates(2)
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("limit not applied: got %d rows, want 2", len(rows))
	}
	if rows[0].ReadBps+rows[0].WriteBps < rows[1].ReadBps+rows[1].WriteBps {
		t.Errorf("rows not sorted busiest-first: %+v", rows)
	}
	if rows[0].WriteBps != 100<<20 {
		t.Errorf("busiest row is not the 100 MB/s writer: %+v", rows[0])
	}
}

func TestRatesIgnoresCounterGoingBackwards(t *testing.T) {
	// PID reuse gives an existing key a fresh, smaller counter set. Reporting
	// the difference would underflow into an absurd rate.
	pid := uint32(os.Getpid())
	a, _ := newTestAccounting(t, 10*time.Second,
		[]ioSample{sample(pid, 35, "old", 500<<20, 0, 1000, 0)},
		[]ioSample{sample(pid, 35, "new", 4096, 0, 1, 0)},
	)

	if _, err := a.Rates(10); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Rates(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("backwards counter produced a row: %+v", rows)
	}
}

func TestRatesPrunesOnlyIdleDeadProcesses(t *testing.T) {
	alive := uint32(os.Getpid())
	dead := uint32(1 << 30) // far above any pid_max

	// Round 2: the dead pid still has traffic, so it must be reported and kept.
	// Round 3: it is idle and gone, so it can be pruned.
	a, tracer := newTestAccounting(t, 10*time.Second,
		[]ioSample{sample(alive, 35, "a", 0, 0, 0, 0), sample(dead, 35, "d", 0, 0, 0, 0)},
		[]ioSample{sample(alive, 35, "a", 1<<20, 0, 1, 0), sample(dead, 35, "d", 1<<20, 0, 1, 0)},
		[]ioSample{sample(alive, 35, "a", 2<<20, 0, 2, 0), sample(dead, 35, "d", 1<<20, 0, 1, 0)},
	)

	if _, err := a.Rates(10); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Rates(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("round 2: got %d rows, want 2 (dead process's last burst must still show)", len(rows))
	}
	if len(tracer.forgot) != 0 {
		t.Fatalf("round 2 pruned an active key: %+v", tracer.forgot)
	}

	if _, err := a.Rates(10); err != nil {
		t.Fatal(err)
	}
	if len(tracer.forgot) != 1 || tracer.forgot[0].PID != dead {
		t.Fatalf("round 3 forgot %+v, want exactly the dead pid %d", tracer.forgot, dead)
	}
}

func TestParseMountinfo(t *testing.T) {
	// Real-shaped lines: a zfs mount with an optional field, one without, a
	// second mount of the same dataset, and non-zfs mounts that must be
	// ignored -- including one whose *source* is the word "zfs".
	content := `25 30 0:24 / /proc rw,nosuid - proc proc rw
36 25 0:35 / /tank rw,noatime shared:1 - zfs tank rw,xattr,noacl
37 25 0:36 / /var/lib/db rw,noatime - zfs tank/db rw,xattr,noacl
38 25 0:36 / /mnt/db-again rw,noatime - zfs tank/db rw,xattr,noacl
39 25 8:1 / /boot rw,relatime - ext4 /dev/sda1 rw
40 25 0:99 / /weird rw - overlay zfs rw
`
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	byDev, err := parseMountinfo(path)
	if err != nil {
		t.Fatalf("parseMountinfo: %v", err)
	}

	want := map[uint32]string{
		35: "tank",    // MKDEV(0,35)
		36: "tank/db", // first mount wins over /mnt/db-again
	}
	if len(byDev) != len(want) {
		t.Fatalf("got %d zfs mounts (%v), want %d", len(byDev), byDev, len(want))
	}
	for dev, name := range want {
		if byDev[dev] != name {
			t.Errorf("dev %d = %q, want %q", dev, byDev[dev], name)
		}
	}
}

func TestParseDev(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"0:35", 35, true},           // anonymous device, as ZFS uses
		{"8:1", 8<<20 | 1, true},     // a real block device
		{"259:0", 259<<20 | 0, true}, // major above 255
		{"0:1048575", 0xfffff, true}, // largest minor that fits
		{"bad", 0, false},            //
		{"x:1", 0, false},            //
		{"8:y", 0, false},            //
	}
	for _, c := range cases {
		got, ok := parseDev(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseDev(%q) = %d, %v; want %d, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestUnknownDatasetRoundTripsDev(t *testing.T) {
	dev, _ := parseDev("0:35")
	if got, want := unknownDataset(dev), "dev 0:35"; got != want {
		t.Errorf("unknownDataset = %q, want %q", got, want)
	}
}

func TestRate(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B/s"},
		{512, "512 B/s"},
		{1024, "1.0 KB/s"},
		{1536, "1.5 KB/s"},
		{5 << 20, "5.0 MB/s"},
		{3 << 30, "3.0 GB/s"},
		{2 << 40, "2.0 TB/s"},
	}
	for _, c := range cases {
		if got := rate(c.in); got != c.want {
			t.Errorf("rate(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTopProcessesZeroDisablesMonitor(t *testing.T) {
	cfg := DefaultConfig()
	cfg.TopProcesses = 0

	m := startIOMonitor(cfg)
	defer m.Close()

	rows, errText, ready := m.Snapshot()
	if rows != nil || ready {
		t.Errorf("disabled monitor produced data: rows=%v ready=%v", rows, ready)
	}
	if errText == "" {
		t.Error("disabled monitor should explain itself in the dashboard")
	}
}
