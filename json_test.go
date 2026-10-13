package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// decode renders a report as JSON and unmarshals it back into a generic map,
// so assertions are about the wire format a consumer sees rather than the Go
// structs that produced it.
func decode(t *testing.T, r report, indent bool) (map[string]any, string) {
	t.Helper()

	var b strings.Builder
	if err := RenderJSON(&b, r, indent); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(b.String()), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, b.String())
	}
	return got, b.String()
}

// scale is the expected byte count for a ZFS size: v followed by pow
// multiplications by 1024. Written as a function, not a constant expression,
// because the rounding of e.g. 4.10T has to happen in float64 to match what
// parseSizeBytes does with the same string.
func scale(v float64, pow int) uint64 {
	for ; pow > 0; pow-- {
		v *= 1024
	}
	return uint64(v)
}

func fullReport() report {
	return report{
		Status: Status{
			Pools: []Pool{
				{Name: "tank", Health: "ONLINE", Size: "21.8T", Alloc: "12.4T", Cap: "56%", Frag: "8%"},
			},
			Datasets: []Dataset{
				{Name: "tank/db", Used: "4.10T", Avail: "9.4T", Ratio: "1.02x"},
			},
			Scrubs: []Scrub{
				{Pool: "tank", Percent: "32.14%", ToGo: "01:12:33"},
			},
		},
		At:         time.Date(2026, 2, 14, 9, 31, 7, 0, time.UTC),
		ProcIO:     []ProcIO{{PID: 1841, Command: "postgres", Dataset: "tank/db", WriteBps: 98784247, WriteIOPS: 812}},
		ProcNote:   "1s sample",
		ProcWindow: time.Second,
		Filter:     []string{"tank"},
	}
}

func TestRenderJSONSnapshot(t *testing.T) {
	got, raw := decode(t, fullReport(), true)

	if got["time"] != "2026-02-14T09:31:07Z" {
		t.Errorf("time = %v, want RFC3339 UTC", got["time"])
	}

	pool := got["pools"].([]any)[0].(map[string]any)
	// The display strings survive untouched, and the derived numbers sit
	// beside them rather than replacing them.
	if pool["size"] != "21.8T" {
		t.Errorf("size = %v, want 21.8T", pool["size"])
	}
	if want := float64(scale(21.8, 4)); pool["size_bytes"] != want {
		t.Errorf("size_bytes = %v, want %v", pool["size_bytes"], want)
	}
	if pool["capacity_percent"] != 56.0 {
		t.Errorf("capacity_percent = %v, want 56", pool["capacity_percent"])
	}
	if pool["fragmentation_percent"] != 8.0 {
		t.Errorf("fragmentation_percent = %v, want 8", pool["fragmentation_percent"])
	}

	scrub := got["scrubs"].([]any)[0].(map[string]any)
	if scrub["percent_done"] != 32.14 {
		t.Errorf("percent_done = %v, want 32.14", scrub["percent_done"])
	}
	if scrub["time_remaining_seconds"] != 4353.0 { // 1h 12m 33s
		t.Errorf("time_remaining_seconds = %v, want 4353", scrub["time_remaining_seconds"])
	}

	ds := got["datasets"].([]any)[0].(map[string]any)
	if ds["compress_ratio"] != 1.02 {
		t.Errorf("compress_ratio = %v, want 1.02", ds["compress_ratio"])
	}

	pio := got["process_io"].(map[string]any)
	if pio["available"] != true || pio["window_seconds"] != 1.0 {
		t.Errorf("process_io = %v, want available over a 1s window", pio)
	}
	proc := pio["processes"].([]any)[0].(map[string]any)
	if proc["pid"] != 1841.0 || proc["write_bytes_per_sec"] != 98784247.0 || proc["write_iops"] != 812.0 {
		t.Errorf("process row = %v", proc)
	}
	if proc["read_bytes_per_sec"] != 0.0 {
		// Zero is a measurement, not a missing value, so it is always present.
		t.Errorf("read_bytes_per_sec = %v, want 0", proc["read_bytes_per_sec"])
	}

	if !strings.Contains(raw, "\n  \"pools\"") {
		t.Errorf("indented output should be multi-line:\n%s", raw)
	}
}

// A consumer should be able to range over every list without a nil check, and
// should not see a key for something zpulse does not know.
func TestRenderJSONEmptyReportHasEmptyLists(t *testing.T) {
	got, raw := decode(t, report{At: time.Now()}, true)

	for _, key := range []string{"pools", "scrubs", "datasets", "pools_filter"} {
		list, ok := got[key].([]any)
		if !ok || list == nil {
			t.Errorf("%s = %v, want an empty list", key, got[key])
		}
	}
	if procs := got["process_io"].(map[string]any)["processes"]; procs == nil {
		t.Errorf("processes = null, want an empty list")
	}
	if _, ok := got["error"]; ok {
		t.Errorf("error key present on a clean run:\n%s", raw)
	}
}

func TestRenderJSONReportsFailures(t *testing.T) {
	r := report{
		Status:    Status{Err: "zpool list: exit status 1"},
		At:        time.Now(),
		ProcIOErr: "needs CAP_BPF",
	}
	got, _ := decode(t, r, true)

	if got["error"] != "zpool list: exit status 1" {
		t.Errorf("error = %v, want the collection failure", got["error"])
	}
	pio := got["process_io"].(map[string]any)
	if pio["available"] != false || pio["error"] != "needs CAP_BPF" {
		t.Errorf("process_io = %v, want unavailable with a reason", pio)
	}
	// The window describes numbers that aren't there.
	if _, ok := pio["window_seconds"]; ok {
		t.Errorf("window_seconds present without a sample: %v", pio)
	}
}

// Watch mode streams one object per line, so nothing may be indented and every
// frame has to end with exactly one newline.
func TestRenderJSONCompactIsOneLine(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2; i++ {
		if err := RenderJSON(&b, fullReport(), false); err != nil {
			t.Fatalf("RenderJSON: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), b.String())
	}
	for i, line := range lines {
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Errorf("line %d is not a complete object: %v", i, err)
		}
	}
}

// The first frame of `watch` has a baseline and nothing to compare it to, so
// an empty table there has to be distinguishable from an idle pool.
func TestRenderJSONFlagsTheWarmUpFrame(t *testing.T) {
	warm, _ := decode(t, report{At: time.Now(), ProcWarm: true}, true)
	if warm["process_io"].(map[string]any)["warming_up"] != true {
		t.Errorf("warming_up missing on the first frame: %v", warm["process_io"])
	}

	idle, _ := decode(t, report{At: time.Now()}, true)
	if _, ok := idle["process_io"].(map[string]any)["warming_up"]; ok {
		t.Errorf("warming_up present on a settled frame: %v", idle["process_io"])
	}
}

func TestParseSizeBytes(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"0B", 0, true},
		{"512", 512, true},
		{"96K", scale(96, 1), true},
		{"4.10T", scale(4.10, 4), true},
		{"1.5P", scale(1.5, 5), true},
		{"96k", scale(96, 1), true}, // zfs prints upper case, but be forgiving
		{"-", 0, false},             // a pool that does not report the value
		{"", 0, false},
		{"none", 0, false},
		{"12.4Q", 0, false}, // a suffix we don't know is not a guess
	}

	for _, c := range cases {
		got := parseSizeBytes(c.in)
		switch {
		case c.ok && got == nil:
			t.Errorf("parseSizeBytes(%q) = nil, want %d", c.in, c.want)
		case !c.ok && got != nil:
			t.Errorf("parseSizeBytes(%q) = %d, want nil", c.in, *got)
		case c.ok && *got != c.want:
			t.Errorf("parseSizeBytes(%q) = %d, want %d", c.in, *got, c.want)
		}
	}
}

func TestParseRatioAndToGo(t *testing.T) {
	if got := parseRatio("1.42x"); got == nil || *got != 1.42 {
		t.Errorf("parseRatio(1.42x) = %v, want 1.42", got)
	}
	if got := parseRatio("-"); got != nil {
		t.Errorf("parseRatio(-) = %v, want nil", *got)
	}

	// Hours run past 24 on a long scrub rather than rolling into a day field.
	if got := parseToGoSeconds("36:00:01"); got == nil || *got != 36*3600+1 {
		t.Errorf("parseToGoSeconds(36:00:01) = %v, want 129601", got)
	}
	for _, in := range []string{"", "soon", "00:30", "aa:bb:cc"} {
		if got := parseToGoSeconds(in); got != nil {
			t.Errorf("parseToGoSeconds(%q) = %d, want nil", in, *got)
		}
	}
}
