package main

// Terminal rendering.
//
// Everything zpulse prints goes through this file: a small column-aligned
// table writer, a set of ANSI colours that switch themselves off when stdout
// is not a terminal, and the section layouts for pools, scrubs, process I/O
// and datasets.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ANSI escape codes. Only ones with wide terminal support are used.
const (
	ansiReset  = "\033[0m"
	ansiBold   = "\033[1m"
	ansiDim    = "\033[2m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
)

// palette wraps text in ANSI codes, or leaves it alone when colour is off.
//
// Keeping the on/off decision in one value means the layout code never has to
// ask whether colour is enabled -- the widths it measures are the same either
// way, because padding happens before wrapping.
type palette struct{ enabled bool }

func (p palette) paint(code, s string) string {
	if !p.enabled || code == "" || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (p palette) bold(s string) string  { return p.paint(ansiBold, s) }
func (p palette) dim(s string) string   { return p.paint(ansiDim, s) }
func (p palette) red(s string) string   { return p.paint(ansiRed, s) }
func (p palette) green(s string) string { return p.paint(ansiGreen, s) }

// isTerminal reports whether f is attached to a terminal rather than a pipe or
// a file, which is what decides colour and whether watch mode may redraw.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// colorEnabled resolves the colour setting: an explicit --color/--no-color
// wins, then NO_COLOR (https://no-color.org), then whether we have a terminal.
func colorEnabled(force, disable bool, out *os.File) bool {
	switch {
	case disable:
		return false
	case force:
		return true
	case os.Getenv("NO_COLOR") != "":
		return false
	default:
		return isTerminal(out)
	}
}

// align is a column's horizontal alignment. Numbers read better right-aligned.
type align int

const (
	alignLeft align = iota
	alignRight
)

type column struct {
	title string
	align align
}

// cell is one table cell: the text used for width calculation, plus the colour
// applied to it afterwards. The two are kept apart so escape codes never end
// up counted as visible characters.
type cell struct {
	text  string
	color string
}

func txt(s string) cell { return cell{text: s} }

func colored(s, code string) cell { return cell{text: s, color: code} }

// writeTable prints a column-aligned table. Column widths come from the widest
// title or cell, so nothing is ever truncated.
func writeTable(w io.Writer, p palette, indent string, cols []column, rows [][]cell) {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = width(c.title)
	}
	for _, row := range rows {
		for i, c := range row {
			if i < len(widths) && width(c.text) > widths[i] {
				widths[i] = width(c.text)
			}
		}
	}

	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = pad(c.title, widths[i]-width(c.title), c.align)
	}
	fmt.Fprintln(w, indent+p.dim(strings.TrimRight(strings.Join(header, "  "), " ")))

	for _, row := range rows {
		out := make([]string, 0, len(row))
		for i, c := range row {
			a := alignLeft
			if i < len(cols) {
				a = cols[i].align
			}
			to := 0
			if i < len(widths) {
				to = widths[i]
			}
			// Colour the text, pad around it: escape codes inside the padding
			// would be counted as width and skew every column after this one.
			out = append(out, pad(p.paint(c.color, c.text), to-width(c.text), a))
		}
		fmt.Fprintln(w, indent+strings.TrimRight(strings.Join(out, "  "), " "))
	}
}

// pad adds n spaces to s on the side that a calls for.
func pad(s string, n int, a align) string {
	if n <= 0 {
		return s
	}
	fill := strings.Repeat(" ", n)
	if a == alignRight {
		return fill + s
	}
	return s + fill
}

// width is a cell's printed width. Cell text is plain -- colour is applied
// after padding -- so counting runes is enough; the box-drawing and em-dash
// glyphs zpulse prints are all one column wide.
func width(s string) int { return utf8.RuneCountInString(s) }

// section prints a heading. Sections are separated by a blank line above.
func section(w io.Writer, p palette, title string, note string) {
	fmt.Fprintln(w)
	line := p.bold(strings.ToUpper(title))
	if note != "" {
		line += "  " + p.dim(note)
	}
	fmt.Fprintln(w, line)
}

func empty(w io.Writer, p palette, msg string) {
	fmt.Fprintln(w, "  "+p.dim(msg))
}

const barWidth = 16

// bar draws a usage meter for a percentage string like "56%", returning the
// meter and the colour to paint it. The two are separate so the table can
// measure the meter's width without escape codes in the way. Unparseable input
// (an older zpool printing "-") yields an empty meter rather than a misleading
// full or zero one.
func bar(percent string) (string, string) {
	pct, ok := parsePercent(percent)
	if !ok {
		return strings.Repeat("·", barWidth), ansiDim
	}
	return meter(pct), capacityColor(pct)
}

// meter renders pct as a fixed-width filled bar.
func meter(pct float64) string {
	filled := int(float64(barWidth)*pct/100 + 0.5)
	if filled > barWidth {
		filled = barWidth
	}
	if filled < 0 {
		filled = 0
	}
	return strings.Repeat("█", filled) + strings.Repeat("·", barWidth-filled)
}

// capacityColor flags pools filling up. ZFS performance falls off well before
// a pool is actually full, so the warning starts at 80%.
func capacityColor(pct float64) string {
	switch {
	case pct >= 90:
		return ansiRed
	case pct >= 80:
		return ansiYellow
	default:
		return ansiGreen
	}
}

func parsePercent(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// healthColor maps a pool health string to its ANSI colour. Anything that is
// not ONLINE is a problem worth seeing from across the room.
func healthColor(health string) string {
	switch health {
	case "ONLINE":
		return ansiGreen
	case "DEGRADED":
		return ansiYellow
	case "FAULTED", "UNAVAIL", "REMOVED":
		return ansiRed
	case "OFFLINE":
		return ansiDim
	default:
		return ""
	}
}

// rate formats a bytes-per-second figure the way zpool and zfs format sizes, so
// the process table reads consistently with the rest of the output.
func rate(bps uint64) string {
	const unit = 1024
	if bps < unit {
		return fmt.Sprintf("%d B/s", bps)
	}
	div, exp := uint64(unit), 0
	for n := bps / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB/s", float64(bps)/float64(div), "KMGTP"[exp])
}

// report is everything one screen of output needs.
type report struct {
	Status
	At         time.Time
	ProcIO     []ProcIO
	ProcIOErr  string        // why there is no process table, if there isn't one
	ProcNote   string        // shown next to the PROCESS I/O heading
	ProcWindow time.Duration // the window those rates cover, for --json
	ProcWarm   bool          // true while the first sample is only a baseline
	Filter     []string      // the pools asked for, for the header line
}

// Render writes a full report.
func Render(w io.Writer, p palette, r report) {
	fmt.Fprintf(w, "%s  %s\n",
		p.paint(ansiBold+ansiCyan, "zpulse"),
		p.dim(r.At.Format("2006-01-02 15:04:05")+"  ·  "+poolsLabel(r.Filter)))

	if r.Err != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, p.red("! "+r.Err))
	}

	renderPools(w, p, r.Pools)
	renderScrubs(w, p, r.Scrubs)
	renderProcIO(w, p, r)
	renderDatasets(w, p, r.Datasets)
}

func renderPools(w io.Writer, p palette, pools []Pool) {
	section(w, p, "pools", "")
	if len(pools) == 0 {
		empty(w, p, "No pools found.")
		return
	}

	cols := []column{
		{"NAME", alignLeft},
		{"HEALTH", alignLeft},
		{"ALLOC", alignRight},
		{"SIZE", alignRight},
		{"CAP", alignRight},
		{"FRAG", alignRight},
		{"USAGE", alignLeft},
	}

	rows := make([][]cell, 0, len(pools))
	for _, pool := range pools {
		rows = append(rows, []cell{
			colored(pool.Name, ansiBold),
			colored(pool.Health, healthColor(pool.Health)),
			txt(pool.Alloc),
			txt(pool.Size),
			txt(pool.Cap),
			txt(pool.Frag),
			colored(bar(pool.Cap)),
		})
	}
	writeTable(w, p, "  ", cols, rows)
}

func renderScrubs(w io.Writer, p palette, scrubs []Scrub) {
	section(w, p, "scrubs", "")
	if len(scrubs) == 0 {
		empty(w, p, "No scrubs running.")
		return
	}

	cols := []column{
		{"POOL", alignLeft},
		{"PROGRESS", alignLeft},
		{"DONE", alignRight},
		{"REMAINING", alignRight},
	}

	rows := make([][]cell, 0, len(scrubs))
	for _, s := range scrubs {
		rows = append(rows, []cell{
			colored(s.Pool, ansiBold),
			colored(scrubBar(s.Percent), ansiBlue),
			txt(dash(s.Percent)),
			txt(dash(s.ToGo)),
		})
	}
	writeTable(w, p, "  ", cols, rows)
}

// scrubBar is the same meter without the capacity colouring -- a scrub at 95%
// is good news, not a warning.
func scrubBar(percent string) string {
	pct, ok := parsePercent(percent)
	if !ok {
		return strings.Repeat("·", barWidth)
	}
	return meter(pct)
}

func renderProcIO(w io.Writer, p palette, r report) {
	// The sampling note describes numbers that are not there when the tracer
	// failed, so it goes away with them.
	note := r.ProcNote
	if r.ProcIOErr != "" {
		note = ""
	}
	section(w, p, "process i/o", note)

	if r.ProcIOErr != "" {
		empty(w, p, "Unavailable — "+r.ProcIOErr)
		return
	}
	if len(r.ProcIO) == 0 {
		empty(w, p, "No process read or wrote a ZFS dataset during the sample.")
		return
	}

	cols := []column{
		{"PROCESS", alignLeft},
		{"PID", alignRight},
		{"DATASET", alignLeft},
		{"READ", alignRight},
		{"WRITE", alignRight},
		{"IOPS", alignRight},
	}

	rows := make([][]cell, 0, len(r.ProcIO))
	for _, io := range r.ProcIO {
		rows = append(rows, []cell{
			colored(io.Command, ansiBold),
			colored(strconv.Itoa(io.PID), ansiDim),
			txt(io.Dataset),
			colored(rate(io.ReadBps), busyColor(io.ReadBps)),
			colored(rate(io.WriteBps), busyColor(io.WriteBps)),
			colored(fmt.Sprintf("%dr/%dw", io.ReadIOPS, io.WriteIOPS), ansiDim),
		})
	}
	writeTable(w, p, "  ", cols, rows)
}

// busyColor dims the rates that are effectively zero so the eye lands on the
// column that is actually moving.
func busyColor(bps uint64) string {
	if bps == 0 {
		return ansiDim
	}
	return ansiCyan
}

func renderDatasets(w io.Writer, p palette, datasets []Dataset) {
	section(w, p, "datasets", "")
	if len(datasets) == 0 {
		empty(w, p, "No datasets found.")
		return
	}

	cols := []column{
		{"NAME", alignLeft},
		{"USED", alignRight},
		{"AVAIL", alignRight},
		{"COMPRESS", alignRight},
	}

	rows := make([][]cell, 0, len(datasets))
	for _, d := range datasets {
		rows = append(rows, []cell{
			txt(d.Name),
			txt(d.Used),
			txt(d.Avail),
			colored(d.Ratio, ansiDim),
		})
	}
	writeTable(w, p, "  ", cols, rows)
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func poolsLabel(pools []string) string {
	if len(pools) == 0 {
		return "all pools"
	}
	return strings.Join(pools, ", ")
}
