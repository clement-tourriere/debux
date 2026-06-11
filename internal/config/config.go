// Package config loads optional user defaults from
// $XDG_CONFIG_HOME/debux/config.yaml (or $DEBUX_CONFIG). Flags always win;
// config fills in values the user did not pass.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Config holds user defaults and named tool sets.
//
//	image: ghcr.io/clement-tourriere/debux:latest
//	profile: restricted
//	pull-policy: IfNotPresent
//	terminal: wezterm
//	tools:
//	  python: [python3, py-spy, gdb]
//	  net: [socat, mtr, iperf3]
type Config struct {
	Image      string              `yaml:"image"`
	Profile    string              `yaml:"profile"`
	PullPolicy string              `yaml:"pull-policy"`
	Terminal   string              `yaml:"terminal"`
	Tools      map[string][]string `yaml:"tools"`
}

var (
	once    sync.Once
	loaded  Config
	loadErr error
)

// Path returns the config file location: $DEBUX_CONFIG if set, otherwise
// <user config dir>/debux/config.yaml.
func Path() (string, error) {
	if path := strings.TrimSpace(os.Getenv("DEBUX_CONFIG")); path != "" {
		return path, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving config directory: %w", err)
	}
	return filepath.Join(base, "debux", "config.yaml"), nil
}

// Get returns the loaded config. A missing file yields a zero Config; a
// malformed file is reported once on stderr and otherwise ignored, so a typo
// in the config never blocks debugging.
func Get() Config {
	once.Do(func() {
		loaded, loadErr = load()
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: ignoring debux config: %v\n", loadErr)
		}
	})
	return loaded
}

func load() (Config, error) {
	path, err := Path()
	if err != nil {
		return Config{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// ResolveTools expands --tools values: a value naming a configured tool set
// expands to its packages; anything else is treated as a literal (optionally
// comma-separated) nixpkgs package list.
func ResolveTools(values []string) []string {
	cfg := Get()
	var packages []string
	seen := make(map[string]struct{})
	add := func(pkg string) {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" {
			return
		}
		if _, ok := seen[pkg]; ok {
			return
		}
		seen[pkg] = struct{}{}
		packages = append(packages, pkg)
	}
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if set, ok := cfg.Tools[item]; ok {
				for _, pkg := range set {
					add(pkg)
				}
				continue
			}
			add(item)
		}
	}
	return packages
}
