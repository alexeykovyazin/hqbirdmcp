package ctl

// Config lookup for the commands that read fbmcp.yaml (UX review W1.5).
// The operator should not have to type the path on a standard install.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/hqbirdsetup"
)

// configHelp is the lookup order, for the usage texts.
const configHelp = "config: --config FILE, else $FBMCP_CONFIG, else ./fbmcp.yaml, else fbmcp.yaml next to the program, else the HQbird default"

// exePath and defaultConfig are variables so the tests can move them.
var (
	exePath       = os.Executable
	defaultConfig = func() string { return hqbirdsetup.DefaultPaths(runtime.GOOS, "").Config }
)

// resolveConfig returns the config file to use. explicit (--config or a
// positional path) wins and is not checked here: config.Load reports it.
func resolveConfig(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := strings.TrimSpace(os.Getenv("FBMCP_CONFIG")); env != "" {
		return env, nil
	}
	tried := []string{"fbmcp.yaml"}
	if exe, err := exePath(); err == nil {
		tried = append(tried, filepath.Join(filepath.Dir(exe), "fbmcp.yaml"))
	}
	tried = append(tried, defaultConfig())
	for _, p := range tried {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no fbmcp.yaml found (tried: %s, and $FBMCP_CONFIG is not set)\nFix: add --config <path to fbmcp.yaml>, or set FBMCP_CONFIG", strings.Join(tried, ", "))
}

// takeConfigFlag removes --config FILE / --config=FILE from args.
func takeConfigFlag(args []string) (string, []string) {
	var cfg string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "--config" || args[i] == "-config") && i+1 < len(args):
			cfg = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--config="):
			cfg = strings.TrimPrefix(args[i], "--config=")
		default:
			rest = append(rest, args[i])
		}
	}
	return cfg, rest
}

// loadConfig resolves and loads the config and says which file it used
// (on stderr, so stdout keeps only the report).
func loadConfig(explicit string) (*config.Config, string, error) {
	path, err := resolveConfig(explicit)
	if err != nil {
		return nil, "", err
	}
	fmt.Fprintln(os.Stderr, "using config", path)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, path, err
	}
	return cfg, path, nil
}
