package main

import (
	"testing"
	"time"
)

func TestParseFlagsDefaultsToConfig(t *testing.T) {
	cfg := Config{RefreshInterval: 30, Pools: []string{"tank"}, TopProcesses: 5}

	opts, err := parseFlags("status", cfg, nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.interval != 30*time.Second {
		t.Errorf("interval = %v, want 30s", opts.interval)
	}
	if len(opts.cfg.Pools) != 1 || opts.cfg.Pools[0] != "tank" {
		t.Errorf("pools = %v, want [tank]", opts.cfg.Pools)
	}
	if opts.cfg.TopProcesses != 5 {
		t.Errorf("top = %d, want 5", opts.cfg.TopProcesses)
	}
	if !opts.datasets {
		t.Error("datasets should be shown by default")
	}
}

func TestParseFlagsOverrideConfig(t *testing.T) {
	cfg := Config{RefreshInterval: 30, Pools: []string{"tank"}, TopProcesses: 5}
	args := []string{"--pools", "backup, scratch", "--top", "3", "--interval", "2s", "--no-datasets"}

	opts, err := parseFlags("watch", cfg, args)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got := opts.cfg.Pools; len(got) != 2 || got[0] != "backup" || got[1] != "scratch" {
		t.Errorf("pools = %v, want [backup scratch]", got)
	}
	if opts.cfg.TopProcesses != 3 {
		t.Errorf("top = %d, want 3", opts.cfg.TopProcesses)
	}
	if opts.interval != 2*time.Second {
		t.Errorf("interval = %v, want 2s", opts.interval)
	}
	// The I/O monitor samples on the config's interval, so --interval has to
	// carry through to it or the rates would cover the wrong window.
	if opts.cfg.RefreshInterval != 2 {
		t.Errorf("cfg.RefreshInterval = %d, want 2", opts.cfg.RefreshInterval)
	}
	if opts.datasets {
		t.Error("--no-datasets should hide datasets")
	}
}

// A sub-second interval still has to leave the monitor a whole number of
// seconds it can tick on.
func TestParseFlagsKeepsMonitorIntervalAtLeastASecond(t *testing.T) {
	opts, err := parseFlags("watch", DefaultConfig(), []string{"--interval", "200ms"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.cfg.RefreshInterval < 1 {
		t.Errorf("cfg.RefreshInterval = %d, want at least 1", opts.cfg.RefreshInterval)
	}
}

func TestParseFlagsRejectsBadInput(t *testing.T) {
	cases := [][]string{
		{"--interval", "0"},
		{"--sample", "-1s"},
		{"tank"}, // a stray positional, most likely a mistyped --pools
	}
	for _, args := range cases {
		if _, err := parseFlags("status", DefaultConfig(), args); err == nil {
			t.Errorf("parseFlags(%v) accepted bad input", args)
		}
	}
}

func TestSplitList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  ", nil},
		{"tank", []string{"tank"}},
		{"tank,backup", []string{"tank", "backup"}},
		{" tank , backup ,", []string{"tank", "backup"}},
	}
	for _, c := range cases {
		got := splitList(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitList(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitList(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}
}

func TestSampleOnceReportsDisabledTracer(t *testing.T) {
	opts := options{cfg: Config{TopProcesses: 0}, sample: time.Millisecond}

	rows, errText, note := sampleOnce(opts)
	if rows != nil {
		t.Errorf("disabled sampling produced rows: %v", rows)
	}
	if errText == "" {
		t.Error("disabled sampling should explain itself in place of the table")
	}
	if note != "" {
		t.Errorf("note = %q, want empty", note)
	}
}
