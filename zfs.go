package main

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// This file is the only place zpulse talks to the system. Every command here is
// read-only: `zpool list`, `zfs list`, and `zpool status`. zpulse never writes
// to a pool.

// Pool is a single ZFS pool as shown in the overview.
type Pool struct {
	Name   string
	Health string
	Size   string // total size, e.g. "21.8T"
	Alloc  string // allocated/used, e.g. "12.4T"
	Cap    string // capacity used, e.g. "56%"
	Frag   string // fragmentation, e.g. "8%"
}

// Dataset is a single ZFS dataset in the dataset table.
type Dataset struct {
	Name  string
	Used  string
	Avail string
	Ratio string // compression ratio, e.g. "1.42x"
}

// Scrub describes a scrub currently running on a pool.
type Scrub struct {
	Pool    string
	Percent string // e.g. "60.00%"
	ToGo    string // estimated time remaining, e.g. "00:30:00"
}

// Status is the full snapshot zpulse prints.
type Status struct {
	Pools    []Pool
	Datasets []Dataset
	Scrubs   []Scrub
	Err      string // populated when a command fails, shown above the tables
}

const commandTimeout = 8 * time.Second

// run executes a command with a timeout and returns its stdout.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// Collect gathers the whole snapshot. A failure in any single command is
// recorded in Status.Err but does not stop the others.
func Collect(pools []string) Status {
	var st Status

	p, err := getPools(pools)
	if err != nil {
		st.Err = "zpool list: " + err.Error()
	}
	st.Pools = p

	d, err := getDatasets(pools)
	if err != nil && st.Err == "" {
		st.Err = "zfs list: " + err.Error()
	}
	st.Datasets = d

	s, err := getScrubs(pools)
	if err != nil && st.Err == "" {
		st.Err = "zpool status: " + err.Error()
	}
	st.Scrubs = s

	return st
}

// getPools runs `zpool list` and parses the tab-separated output.
func getPools(pools []string) ([]Pool, error) {
	args := []string{"list", "-H", "-o", "name,health,size,alloc,cap,frag"}
	args = append(args, pools...)

	out, err := run("zpool", args...)
	if err != nil {
		return nil, err
	}

	var result []Pool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		result = append(result, Pool{
			Name:   f[0],
			Health: f[1],
			Size:   f[2],
			Alloc:  f[3],
			Cap:    f[4],
			Frag:   f[5],
		})
	}
	return result, nil
}

// getDatasets runs `zfs list` and parses the tab-separated output.
func getDatasets(pools []string) ([]Dataset, error) {
	args := []string{"list", "-H", "-o", "name,used,avail,compressratio"}
	if len(pools) > 0 {
		// Restrict to (and recurse into) the configured pools.
		args = append(args, "-r")
		args = append(args, pools...)
	}

	out, err := run("zfs", args...)
	if err != nil {
		return nil, err
	}

	var result []Dataset
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 4 {
			continue
		}
		result = append(result, Dataset{
			Name:  f[0],
			Used:  f[1],
			Avail: f[2],
			Ratio: f[3],
		})
	}
	return result, nil
}

var (
	poolLineRe    = regexp.MustCompile(`^\s*pool:\s*(\S+)`)
	percentDoneRe = regexp.MustCompile(`([\d.]+)%\s+done`)
	toGoRe        = regexp.MustCompile(`,\s*(\S+)\s+to go`)
)

// getScrubs runs `zpool status` and reports only pools with a scrub in progress.
//
// The `scan:` line format varies across ZFS versions, so rather than match a
// single layout we collect the scan block per pool and pull the "% done" and
// "to go" values out with regexps. Pools that aren't scrubbing are skipped.
func getScrubs(pools []string) ([]Scrub, error) {
	args := []string{"status"}
	args = append(args, pools...)

	out, err := run("zpool", args...)
	if err != nil {
		return nil, err
	}

	var result []Scrub

	var curPool string
	var scanText strings.Builder

	flush := func() {
		if curPool == "" {
			return
		}
		text := scanText.String()
		if strings.Contains(text, "in progress") {
			s := Scrub{Pool: curPool}
			if m := percentDoneRe.FindStringSubmatch(text); m != nil {
				s.Percent = m[1] + "%"
			}
			if m := toGoRe.FindStringSubmatch(text); m != nil {
				s.ToGo = m[1]
			}
			result = append(result, s)
		}
	}

	inScan := false
	for _, line := range strings.Split(out, "\n") {
		if m := poolLineRe.FindStringSubmatch(line); m != nil {
			flush()
			curPool = m[1]
			scanText.Reset()
			inScan = false
			continue
		}

		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "scan:"):
			inScan = true
			scanText.WriteString(strings.TrimPrefix(trimmed, "scan:"))
			scanText.WriteString(" ")
		case inScan && isScanContinuation(line):
			// Indented continuation lines belong to the scan block.
			scanText.WriteString(trimmed)
			scanText.WriteString(" ")
		default:
			inScan = false
		}
	}
	flush()

	return result, nil
}

// isScanContinuation reports whether an indented line continues the scan block
// rather than starting a new `key:` section (config:, errors:, etc.).
func isScanContinuation(line string) bool {
	if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	// New sections look like "config:", "errors:", "status:", etc.
	if first := strings.Fields(trimmed)[0]; strings.HasSuffix(first, ":") {
		return false
	}
	return true
}
