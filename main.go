// Command zpulse is a small read-only web dashboard for ZFS pools.
//
// It shells out to `zpool` and `zfs` with read-only commands, parses the output,
// and serves a page showing pool health/capacity/fragmentation, datasets, and
// running scrubs. It never writes to your pools.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
)

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 || os.Args[1] != "serve" {
		usage()
		os.Exit(2)
	}

	cfg, err := LoadConfig(configPath())
	if err != nil {
		log.Printf("warning: could not read config: %v", err)
	}

	// Flags override the config file. Default to the config value so an
	// unspecified flag leaves the configured/default port untouched.
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", cfg.Port, "port to listen on")
	fs.Parse(os.Args[2:])
	cfg.Port = *port

	if err := Serve(cfg); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: zpulse serve [--port PORT]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Serves the ZFS dashboard. Config is read from")
	fmt.Fprintln(os.Stderr, "/etc/zpulse/config.toml if present.")
}
