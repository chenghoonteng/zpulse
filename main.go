// Command zpulse is a read-only command-line monitor for ZFS pools.
//
// It shells out to `zpool` and `zfs` with read-only commands, parses the
// output, and prints pool health/capacity/fragmentation, datasets, running
// scrubs, and (on an eBPF build) per-process I/O. It never writes to your
// pools.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(0)

	cfg, err := LoadConfig(configPath())
	if err != nil {
		log.Printf("warning: could not read config: %v", err)
	}

	cmd, args := "status", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "status":
		err = runStatus(cfg, args)
	case "watch":
		err = runWatch(cfg, args)
	case "help":
		usage(os.Stdout)
	case "serve":
		log.Fatal("zpulse no longer has a web server; run `zpulse status` or `zpulse watch`")
	default:
		fmt.Fprintf(os.Stderr, "zpulse: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	switch {
	case errors.Is(err, errHelp):
		os.Exit(0)
	case errors.Is(err, errReported):
		// The problem is already on screen as part of the report; repeating
		// it would just push the tables further up the scrollback.
		os.Exit(1)
	case err != nil:
		log.Fatalf("zpulse: %v", err)
	}
}

// errReported marks a failure that has already been printed -- by the report
// itself, or by the flag package. It exists so that zpulse can exit non-zero
// for a script without printing the same message twice.
var errReported = errors.New("reported in output")

// errHelp means -h was passed: usage is already on screen and this is a
// successful run, not a failure.
var errHelp = errors.New("help requested")

func usage(w io.Writer) {
	fmt.Fprint(w, `usage: zpulse [command] [flags]

Commands:
  status    print one snapshot and exit (default)
  watch     redraw the snapshot on an interval until interrupted
  help      show this message

Flags:
  --pools tank,backup   only these pools (default: all)
  --top N               rows in the process I/O table; 0 turns it off
  --sample DURATION     process I/O sample window, status only (default 1s)
  --interval DURATION   redraw interval, watch only (default 10s)
  --no-datasets         skip the dataset table
  --json                machine-readable output instead of tables
  --color / --no-color  force colour on or off (default: on when a terminal)

Config is read from /etc/zpulse/config.toml if present; flags override it.
`)
}

// options are the settings shared by both commands, after flags have been
// layered over the config file.
type options struct {
	cfg      Config
	sample   time.Duration
	interval time.Duration
	datasets bool
	json     bool
	palette  palette
}

// parseFlags registers the common flags on fs, parses args, and folds the
// results into the config. Both commands accept every flag so that a habit
// picked up in one works in the other; the irrelevant one is simply unused.
func parseFlags(name string, cfg Config, args []string) (options, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { usage(fs.Output()) }

	pools := fs.String("pools", strings.Join(cfg.Pools, ","), "comma-separated pools to show (empty = all)")
	top := fs.Int("top", cfg.TopProcesses, "rows in the process I/O table; 0 disables it")
	sample := fs.Duration("sample", time.Second, "process I/O sample window for `status`")
	interval := fs.Duration("interval", time.Duration(cfg.RefreshInterval)*time.Second, "redraw interval for `watch`")
	noDatasets := fs.Bool("no-datasets", false, "skip the dataset table")
	asJSON := fs.Bool("json", false, "print JSON instead of tables")
	forceColor := fs.Bool("color", false, "force colour output")
	noColor := fs.Bool("no-color", false, "disable colour output")

	if err := fs.Parse(args); err != nil {
		// flag has already written the reason and the usage text.
		if errors.Is(err, flag.ErrHelp) {
			return options{}, errHelp
		}
		return options{}, errReported
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *sample <= 0 {
		return options{}, fmt.Errorf("--sample must be positive")
	}
	if *interval <= 0 {
		return options{}, fmt.Errorf("--interval must be positive")
	}

	cfg.Pools = splitList(*pools)
	cfg.TopProcesses = *top
	// The monitor in watch mode samples on the refresh interval, so keep the
	// two in step rather than letting the config value drift from --interval.
	cfg.RefreshInterval = int(interval.Seconds())
	if cfg.RefreshInterval < 1 {
		cfg.RefreshInterval = 1
	}

	return options{
		cfg:      cfg,
		sample:   *sample,
		interval: *interval,
		datasets: !*noDatasets,
		json:     *asJSON,
		palette:  palette{enabled: colorEnabled(*forceColor, *noColor, os.Stdout)},
	}, nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// runStatus prints a single snapshot.
//
// Process I/O rates are differences between two reads of a cumulative counter
// set, so a one-shot run has to sample twice with a window in between. That
// window is short (a second by default) because a person waiting at a prompt
// is not the same as `watch` refreshing in the background.
func runStatus(cfg Config, args []string) error {
	opts, err := parseFlags("status", cfg, args)
	if err != nil {
		return err
	}

	rows, procErr, note := sampleOnce(opts)

	st := Collect(opts.cfg.Pools)
	if !opts.datasets {
		st.Datasets = nil
	}

	rep := report{
		Status:     st,
		At:         time.Now(),
		ProcIO:     rows,
		ProcIOErr:  procErr,
		ProcNote:   note,
		ProcWindow: opts.sample,
		Filter:     opts.cfg.Pools,
	}

	if opts.json {
		// A failed command is reported inside the object, as `error`, so the
		// consumer gets one parseable thing either way.
		if err := RenderJSON(os.Stdout, rep, true); err != nil {
			return err
		}
	} else {
		Render(os.Stdout, opts.palette, rep)
	}

	if st.Err != "" {
		return errReported
	}
	return nil
}

// sampleOnce measures per-process I/O over a single window. An unavailable
// tracer is reported in place of the table, never as a fatal error: attribution
// needs privileges and a kernel that the rest of zpulse does not.
func sampleOnce(opts options) (rows []ProcIO, errText, note string) {
	if opts.cfg.TopProcesses <= 0 {
		return nil, "disabled (--top 0)", ""
	}

	tracer, err := newIOTracer()
	if err != nil {
		return nil, err.Error(), ""
	}
	acct := newIOAccounting(tracer)
	defer acct.Close()

	// The first call only establishes a baseline.
	if _, err := acct.Rates(opts.cfg.TopProcesses); err != nil {
		return nil, err.Error(), ""
	}
	time.Sleep(opts.sample)

	rows, err = acct.Rates(opts.cfg.TopProcesses)
	if err != nil {
		return nil, err.Error(), ""
	}
	return rows, "", opts.sample.String() + " sample"
}

// runWatch redraws until interrupted.
func runWatch(cfg Config, args []string) error {
	opts, err := parseFlags("watch", cfg, args)
	if err != nil {
		return err
	}

	// Sampling runs on its own cadence here rather than per redraw, so the
	// process rates cover a full interval even on the first frame after the
	// baseline.
	monitor := startIOMonitor(opts.cfg)
	defer monitor.Close()

	// JSON output is a stream for another program to read, so none of the
	// terminal redrawing applies to it even when stdout is a terminal.
	tty := isTerminal(os.Stdout) && !opts.json
	if tty {
		fmt.Print("\033[?25l") // hide the cursor while we redraw
		defer fmt.Print("\033[?25h")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	ticker := time.NewTicker(opts.interval)
	defer ticker.Stop()

	for {
		draw(opts, monitor, tty)
		select {
		case <-stop:
			if tty {
				fmt.Print("\033[?25h")
			}
			if !opts.json {
				// Leave the shell prompt on its own line. A JSON stream ends
				// with the last object's newline and nothing after it.
				fmt.Println()
			}
			return nil
		case <-ticker.C:
		}
	}
}

func draw(opts options, monitor *ioMonitor, tty bool) {
	st := Collect(opts.cfg.Pools)
	if !opts.datasets {
		st.Datasets = nil
	}
	rows, procErr, ready := monitor.Snapshot()

	note := opts.interval.String() + " interval"
	warming := procErr == "" && !ready
	if warming {
		// Nothing to show yet: the first sample is only a baseline.
		note = "sampling, first numbers in " + opts.interval.String()
	}

	rep := report{
		Status:     st,
		At:         time.Now(),
		ProcIO:     rows,
		ProcIOErr:  procErr,
		ProcNote:   note,
		ProcWindow: opts.interval,
		ProcWarm:   warming,
		Filter:     opts.cfg.Pools,
	}

	if opts.json {
		// One compact object per interval: newline-delimited JSON, so a reader
		// can consume frames as they arrive instead of waiting for an end.
		if err := RenderJSON(os.Stdout, rep, false); err != nil {
			log.Printf("warning: could not write JSON: %v", err)
		}
		return
	}

	var out strings.Builder
	if tty {
		// Home the cursor and clear to the end of the screen. Clearing as we
		// write, rather than blanking first, avoids the flash of an empty
		// terminal between frames.
		out.WriteString("\033[H\033[J")
	} else {
		out.WriteString("\n")
	}

	Render(&out, opts.palette, rep)

	if tty {
		out.WriteString("\n" + opts.palette.dim("Ctrl-C to quit") + "\n")
	}
	fmt.Print(out.String())
}
