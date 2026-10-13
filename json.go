package main

// Machine-readable output.
//
// `zpulse status --json` prints the same snapshot the tables show, as one JSON
// object a monitoring agent can parse. `zpulse watch --json` prints one
// compact object per interval (newline-delimited JSON) instead of redrawing.
//
// Numbers come from ZFS's own display strings, so they carry ZFS's precision:
// a pool zpool calls "21.8T" is reported as 23969353485516 bytes, not to the
// byte. Percentages and IOPS are exact. Every derived field is omitted rather
// than guessed when the string it comes from is missing or unparseable ("-" on
// a pool that does not report fragmentation, for instance), so a consumer can
// tell "zero" from "not known".

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// jsonReport is the top-level object. Field order here is the key order in the
// output, which keeps a diff of two snapshots readable.
type jsonReport struct {
	Time      string        `json:"time"`
	Pools     []jsonPool    `json:"pools"`
	Scrubs    []jsonScrub   `json:"scrubs"`
	Datasets  []jsonDataset `json:"datasets"`
	ProcessIO jsonProcessIO `json:"process_io"`
	Filter    []string      `json:"pools_filter"`
	Error     string        `json:"error,omitempty"`
}

type jsonPool struct {
	Name          string   `json:"name"`
	Health        string   `json:"health"`
	Size          string   `json:"size"`
	SizeBytes     *uint64  `json:"size_bytes,omitempty"`
	Alloc         string   `json:"alloc"`
	AllocBytes    *uint64  `json:"alloc_bytes,omitempty"`
	Capacity      string   `json:"capacity"`
	CapacityPct   *float64 `json:"capacity_percent,omitempty"`
	Fragmentation string   `json:"fragmentation"`
	FragPct       *float64 `json:"fragmentation_percent,omitempty"`
}

type jsonScrub struct {
	Pool          string   `json:"pool"`
	PercentDone   *float64 `json:"percent_done,omitempty"`
	TimeRemaining string   `json:"time_remaining,omitempty"`
	SecsRemaining *uint64  `json:"time_remaining_seconds,omitempty"`
}

type jsonDataset struct {
	Name          string   `json:"name"`
	Used          string   `json:"used"`
	UsedBytes     *uint64  `json:"used_bytes,omitempty"`
	Avail         string   `json:"avail"`
	AvailBytes    *uint64  `json:"avail_bytes,omitempty"`
	CompressRatio *float64 `json:"compress_ratio,omitempty"`
}

// jsonProcessIO carries the table or the reason there isn't one. Attribution
// needs privileges the rest of zpulse does not, so "unavailable" is an
// ordinary outcome here, not an error in the run.
type jsonProcessIO struct {
	Available     bool       `json:"available"`
	Error         string     `json:"error,omitempty"`
	WarmingUp     bool       `json:"warming_up,omitempty"`
	WindowSeconds float64    `json:"window_seconds,omitempty"`
	Processes     []jsonProc `json:"processes"`
}

type jsonProc struct {
	PID       int    `json:"pid"`
	Command   string `json:"command"`
	Dataset   string `json:"dataset"`
	ReadBps   uint64 `json:"read_bytes_per_sec"`
	WriteBps  uint64 `json:"write_bytes_per_sec"`
	ReadIOPS  uint64 `json:"read_iops"`
	WriteIOPS uint64 `json:"write_iops"`
}

// RenderJSON writes a report as JSON. Indented output is for a person running
// `zpulse status --json` at a prompt; compact output is one line per snapshot,
// for `zpulse watch --json` piped into something.
func RenderJSON(w io.Writer, r report, indent bool) error {
	enc := json.NewEncoder(w)
	if indent {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(jsonify(r))
}

// jsonify converts a report into its wire form.
func jsonify(r report) jsonReport {
	out := jsonReport{
		Time:     r.At.Format(time.RFC3339),
		Pools:    make([]jsonPool, 0, len(r.Pools)),
		Scrubs:   make([]jsonScrub, 0, len(r.Scrubs)),
		Datasets: make([]jsonDataset, 0, len(r.Datasets)),
		Filter:   r.Filter,
		Error:    r.Err,
	}
	// Lists are always lists, never null: a consumer should be able to range
	// over them without a nil check.
	if out.Filter == nil {
		out.Filter = []string{}
	}

	for _, p := range r.Pools {
		out.Pools = append(out.Pools, jsonPool{
			Name:          p.Name,
			Health:        p.Health,
			Size:          p.Size,
			SizeBytes:     parseSizeBytes(p.Size),
			Alloc:         p.Alloc,
			AllocBytes:    parseSizeBytes(p.Alloc),
			Capacity:      p.Cap,
			CapacityPct:   parsePercentPtr(p.Cap),
			Fragmentation: p.Frag,
			FragPct:       parsePercentPtr(p.Frag),
		})
	}

	for _, s := range r.Scrubs {
		out.Scrubs = append(out.Scrubs, jsonScrub{
			Pool:          s.Pool,
			PercentDone:   parsePercentPtr(s.Percent),
			TimeRemaining: s.ToGo,
			SecsRemaining: parseToGoSeconds(s.ToGo),
		})
	}

	for _, d := range r.Datasets {
		out.Datasets = append(out.Datasets, jsonDataset{
			Name:          d.Name,
			Used:          d.Used,
			UsedBytes:     parseSizeBytes(d.Used),
			Avail:         d.Avail,
			AvailBytes:    parseSizeBytes(d.Avail),
			CompressRatio: parseRatio(d.Ratio),
		})
	}

	out.ProcessIO = jsonProcessIO{
		Available: r.ProcIOErr == "",
		Error:     r.ProcIOErr,
		Processes: make([]jsonProc, 0, len(r.ProcIO)),
	}
	if r.ProcIOErr == "" {
		out.ProcessIO.WindowSeconds = r.ProcWindow.Seconds()
		// The first frame of `watch` has a baseline but no interval to
		// compare it against yet, so an empty table there means "not yet",
		// not "nothing happened".
		out.ProcessIO.WarmingUp = r.ProcWarm
	}
	for _, p := range r.ProcIO {
		out.ProcessIO.Processes = append(out.ProcessIO.Processes, jsonProc{
			PID:       p.PID,
			Command:   p.Command,
			Dataset:   p.Dataset,
			ReadBps:   p.ReadBps,
			WriteBps:  p.WriteBps,
			ReadIOPS:  p.ReadIOPS,
			WriteIOPS: p.WriteIOPS,
		})
	}

	return out
}

// sizeUnits are the suffixes zpool and zfs print, in power-of-1024 order.
// ZFS uses binary multiples throughout, so "1K" is 1024 bytes.
const sizeUnits = "BKMGTPEZ"

// parseSizeBytes turns a ZFS size like "21.8T", "96K" or "0B" into bytes.
// Anything else -- "-", an empty cell, a future suffix -- yields nil, which
// drops the field from the output rather than reporting a wrong number.
func parseSizeBytes(s string) *uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return nil
	}

	unit := s[len(s)-1]
	digits := s
	exp := 0
	if unit < '0' || unit > '9' {
		i := strings.IndexByte(sizeUnits, byte(unitUpper(unit)))
		if i < 0 {
			return nil
		}
		digits, exp = s[:len(s)-1], i
	}

	v, err := strconv.ParseFloat(digits, 64)
	if err != nil || v < 0 {
		return nil
	}
	for ; exp > 0; exp-- {
		v *= 1024
	}
	// Sizes this large are a parse accident, not a pool.
	if v > 1<<63 {
		return nil
	}
	n := uint64(v)
	return &n
}

func unitUpper(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}

// parsePercentPtr is parsePercent in the shape the JSON fields want.
func parsePercentPtr(s string) *float64 {
	v, ok := parsePercent(s)
	if !ok {
		return nil
	}
	return &v
}

// parseRatio turns a compression ratio like "1.42x" into 1.42.
func parseRatio(s string) *float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "x")
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// parseToGoSeconds turns a scrub's "to go" estimate into seconds. ZFS prints
// it as HH:MM:SS, with the hours running past 24 on a long scrub.
func parseToGoSeconds(s string) *uint64 {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return nil
	}
	var total uint64
	for _, p := range parts {
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil
		}
		total = total*60 + n
	}
	return &total
}
