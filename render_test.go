package main

import (
	"strings"
	"testing"
	"time"
)

// plain is the palette used by tests: colour off, so assertions are about
// layout rather than escape codes.
var plain = palette{}

func TestWriteTableAlignsColumns(t *testing.T) {
	cols := []column{{"NAME", alignLeft}, {"SIZE", alignRight}}
	rows := [][]cell{
		{txt("tank"), txt("21.8T")},
		{txt("scratch"), txt("1.8T")},
	}

	var b strings.Builder
	writeTable(&b, plain, "  ", cols, rows)

	want := []string{
		"  NAME      SIZE",
		"  tank     21.8T",
		"  scratch   1.8T",
	}
	got := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(got), len(want), b.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Colour is applied after padding is measured, so a coloured table has to line
// up exactly the way an uncoloured one does once the escape codes are removed.
func TestWriteTableColorDoesNotShiftColumns(t *testing.T) {
	cols := []column{{"NAME", alignLeft}, {"HEALTH", alignLeft}, {"CAP", alignRight}}
	rows := [][]cell{
		{colored("tank", ansiBold), colored("ONLINE", ansiGreen), txt("56%")},
		{colored("scratch", ansiBold), colored("DEGRADED", ansiYellow), txt("4%")},
	}

	var withColor, without strings.Builder
	writeTable(&withColor, palette{enabled: true}, "  ", cols, rows)
	writeTable(&without, plain, "  ", cols, rows)

	if got := stripANSI(withColor.String()); got != without.String() {
		t.Errorf("coloured layout differs from plain:\n got %q\nwant %q", got, without.String())
	}
}

// Bars and the em-dash are multi-byte, so a width counted in bytes would pad
// them into the next column.
func TestWriteTableMeasuresMultibyteCellsInRunes(t *testing.T) {
	cols := []column{{"PROGRESS", alignLeft}, {"DONE", alignRight}}
	rows := [][]cell{
		{txt(strings.Repeat("█", 4) + strings.Repeat("·", 12)), txt("25%")},
		{txt(strings.Repeat("·", 16)), txt("—")},
	}

	var b strings.Builder
	writeTable(&b, plain, "", cols, rows)

	for i, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if got := width(line); got != 22 { // 16 bar + 2 gap + 4 "DONE"
			t.Errorf("line %d has width %d, want 22: %q", i, got, line)
		}
	}
}

func TestBar(t *testing.T) {
	cases := []struct {
		in        string
		wantFill  int
		wantColor string
	}{
		{"0%", 0, ansiGreen},
		{"50%", 8, ansiGreen},
		{"56%", 9, ansiGreen},
		{"84%", 13, ansiYellow},
		{"94%", 15, ansiRed},
		{"100%", 16, ansiRed},
		{"-", 0, ansiDim}, // older zpool with no value
		{"", 0, ansiDim},
	}
	for _, c := range cases {
		meter, color := bar(c.in)
		if got := strings.Count(meter, "█"); got != c.wantFill {
			t.Errorf("bar(%q) filled %d, want %d", c.in, got, c.wantFill)
		}
		if got := width(meter); got != barWidth {
			t.Errorf("bar(%q) width %d, want %d", c.in, got, barWidth)
		}
		if color != c.wantColor {
			t.Errorf("bar(%q) color = %q, want %q", c.in, color, c.wantColor)
		}
	}
}

func TestRenderShowsEverySection(t *testing.T) {
	var b strings.Builder
	Render(&b, plain, report{
		Status: Status{
			Pools:    []Pool{{"tank", "ONLINE", "21.8T", "12.4T", "56%", "8%"}},
			Scrubs:   []Scrub{{"tank", "32.14%", "01:12:33"}},
			Datasets: []Dataset{{"tank/db", "4.10T", "9.4T", "1.02x"}},
		},
		At:       time.Date(2026, 9, 3, 18, 1, 58, 0, time.UTC),
		ProcIO:   []ProcIO{{1841, "postgres", "tank/db", 0, 98802483, 0, 812}},
		ProcNote: "1s sample",
		Filter:   []string{"tank"},
	})
	out := b.String()

	for _, want := range []string{
		"2026-09-03 18:01:58", "tank",
		"POOLS", "ONLINE", "12.4T", "56%",
		"SCRUBS", "32.14%", "01:12:33",
		"PROCESS I/O", "1s sample", "postgres", "1841", "94.2 MB/s", "0r/812w",
		"DATASETS", "tank/db", "1.02x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestRenderEmptyStateAndError(t *testing.T) {
	var b strings.Builder
	Render(&b, plain, report{
		Status:    Status{Err: "zpool list: exit status 1"},
		At:        time.Now(),
		ProcIOErr: "not compiled in",
	})
	out := b.String()

	for _, want := range []string{
		"! zpool list: exit status 1",
		"No pools found.",
		"No scrubs running.",
		"Unavailable — not compiled in",
		"No datasets found.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestColorEnabled(t *testing.T) {
	t.Setenv("NO_COLOR", "")

	// --no-color wins over --color, and over everything else.
	if colorEnabled(true, true, nil) {
		t.Error("--no-color should beat --color")
	}
	// --color wins over an absent terminal.
	if !colorEnabled(true, false, nil) {
		t.Error("--color should force colour on")
	}

	t.Setenv("NO_COLOR", "1")
	if colorEnabled(false, false, nil) {
		t.Error("NO_COLOR should disable colour")
	}
	if !colorEnabled(true, false, nil) {
		t.Error("an explicit --color should beat NO_COLOR")
	}
}

func TestHealthColor(t *testing.T) {
	if healthColor("ONLINE") != ansiGreen {
		t.Error("ONLINE should be green")
	}
	for _, bad := range []string{"DEGRADED", "FAULTED", "UNAVAIL", "REMOVED"} {
		if c := healthColor(bad); c != ansiRed && c != ansiYellow {
			t.Errorf("%s should stand out, got %q", bad, c)
		}
	}
}

// stripANSI removes escape sequences so a coloured line can be compared with
// its plain equivalent.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // skip the 'm'
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
