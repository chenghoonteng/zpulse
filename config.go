package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config holds the (optional) runtime configuration. Defaults match the README.
type Config struct {
	Port            int
	RefreshInterval int      // seconds
	Pools           []string // empty = show all pools
}

// DefaultConfig returns the built-in defaults used when no config file exists.
func DefaultConfig() Config {
	return Config{
		Port:            8090,
		RefreshInterval: 10,
		Pools:           nil,
	}
}

// configPath returns /etc/zpulse/config.toml.
func configPath() string {
	return "/etc/zpulse/config.toml"
}

// LoadConfig reads the config file if present. Missing files are not an error;
// the defaults are returned instead. Only the three documented keys are read.
//
// This is a deliberately tiny parser for a flat TOML file (key = value, plus a
// simple string array for `pools`). zpulse's config is trivial enough that
// pulling in a full TOML dependency isn't worth it.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Strip trailing inline comments.
		if i := strings.Index(line, "#"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "port":
			if n, err := strconv.Atoi(value); err == nil {
				cfg.Port = n
			}
		case "refresh_interval":
			if n, err := strconv.Atoi(value); err == nil && n > 0 {
				cfg.RefreshInterval = n
			}
		case "pools":
			cfg.Pools = parseStringArray(value)
		}
	}

	return cfg, scanner.Err()
}

// parseStringArray parses a simple TOML string array like ["tank", "backup"].
func parseStringArray(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, `"'`)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
