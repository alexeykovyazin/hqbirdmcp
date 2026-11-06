package ctl

// fbmcpctl apikey (phase 12 B2): make, rotate and list remote-mode API keys.
// A value is printed once (unless --no-show) and never by list.

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/secrets"
)

func cmdAPIKey(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: fbmcpctl apikey new <identity> [--env NAME] [--max-tier N] [--profile autonomous] [--allow-from CIDR,...] [--config fbmcp.yaml] [--secrets-dir DIR] [--no-show]\n       fbmcpctl apikey rotate <identity> [--env NAME | --config fbmcp.yaml] [--secrets-dir DIR] [--no-show]\n       fbmcpctl apikey list [--config fbmcp.yaml]")
		return 2
	}
	switch args[0] {
	case "new":
		return apikeyNew(args[1:])
	case "rotate":
		return apikeyRotate(args[1:])
	case "list":
		return apikeyList(args[1:])
	}
	fmt.Fprintln(os.Stderr, "unknown apikey op:", args[0])
	return 2
}

var nonAlnum = regexp.MustCompile(`[^A-Z0-9]+`)

// defaultKeyEnv: alice → FBMCP_KEY_ALICE, openclaw-1 → FBMCP_KEY_OPENCLAW_1.
func defaultKeyEnv(identity string) string {
	return "FBMCP_KEY_" + strings.Trim(nonAlnum.ReplaceAllString(strings.ToUpper(identity), "_"), "_")
}

// newKeyValue: 32 random bytes, base64url without padding (43 chars).
func newKeyValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// splitIdentity takes the identity name first, flags after it.
func splitIdentity(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func storeKey(dir, name, value string) (string, error) {
	if dir != "" {
		if err := secrets.WriteFile(dir, name, value); err != nil {
			return "", err
		}
		return "file " + dir + string(os.PathSeparator) + name, nil
	}
	if err := secrets.Set(name, value); err != nil {
		return "", fmt.Errorf("keyring: %w", err)
	}
	return "keyring entry fbmcp/" + name + " (current account)", nil
}

func showKey(identity, value string, noShow bool, where string) {
	if noShow {
		fmt.Printf("key for %s stored in %s (value not shown)\n", identity, where)
		return
	}
	fmt.Printf("key for %s stored in %s\n", identity, where)
	fmt.Printf("API key (shown once; give it to the user over a safe channel):\n%s\n", value)
}

func apikeyNew(args []string) int {
	identity, rest := splitIdentity(args)
	fs := flag.NewFlagSet("apikey new", flag.ContinueOnError)
	env := fs.String("env", "", "secret name (default FBMCP_KEY_<IDENTITY>)")
	tier := fs.Int("max-tier", -1, "max tier 0..2 (default: 1, or 0 for --profile autonomous)")
	profile := fs.String("profile", "", "identity profile: autonomous")
	allow := fs.String("allow-from", "", "source addresses (IP or CIDR, comma separated)")
	cfgPath := fs.String("config", "", "fbmcp.yaml: add the identity to it; secrets.dir is the default --secrets-dir")
	dir := fs.String("secrets-dir", "", "store in this folder (fbmcp.yaml secrets.dir) instead of the keyring")
	noShow := fs.Bool("no-show", false, "do not print the value (copy the secret file instead)")
	force := fs.Bool("force", false, "replace an existing key of that name")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if identity == "" && fs.NArg() > 0 {
		identity = fs.Arg(0)
	}
	if identity == "" {
		fmt.Fprintln(os.Stderr, "apikey new: identity name required")
		return 2
	}
	if *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "apikey new:", err)
			return 1
		}
		for _, id := range cfg.Identities {
			if id.Name == identity {
				fmt.Fprintf(os.Stderr, "apikey new: identity %s is already in %s; use apikey rotate --config\n", identity, *cfgPath)
				return 1
			}
		}
		if *dir == "" {
			*dir = cfg.Secrets.Dir
		}
	}
	name := *env
	if name == "" {
		name = defaultKeyEnv(identity)
	}
	id := config.APIIdentity{Name: identity, KeyEnv: name, Profile: *profile}
	if *tier >= 0 {
		id.MaxTier = config.Tier(*tier)
	}
	id.AllowFrom = splitList(*allow)
	if err := id.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "apikey new:", err)
		return 2
	}
	if os.Getenv(name) != "" {
		fmt.Printf("warning: env %s is set in this shell; env wins over the file and the keyring\n", name)
	}
	if !*force {
		secrets.SetDir(*dir)
		if _, src, err := secrets.Lookup(name); err == nil && src != secrets.SourceEnv {
			fmt.Fprintf(os.Stderr, "apikey new: %s already exists (%s); use apikey rotate, or --force\n", name, src)
			return 1
		}
	}
	value, err := newKeyValue()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	where, err := storeKey(*dir, name, value)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apikey new:", err)
		return 1
	}
	showKey(identity, value, *noShow, where)
	if *cfgPath != "" {
		// The key is stored first: an identity without its key would fail
		// remote-mode Validate on the next reload.
		if err := config.RegisterIdentity(*cfgPath, id); err != nil {
			fmt.Fprintf(os.Stderr, "apikey new: the key is stored, but %s was not changed: %v\nadd this identities: entry by hand:\n%s", *cfgPath, err, identityYAML(id))
			return 1
		}
		fmt.Printf("identity %s added to %s\n", identity, *cfgPath)
	} else {
		fmt.Print("\nfbmcp.yaml identities: entry\n" + identityYAML(id))
	}
	if id.Autonomous() && len(id.AllowFrom) == 0 {
		fmt.Println("warning: autonomous identity without --allow-from; doctor --remote will flag it")
	}
	return 0
}

func identityYAML(id config.APIIdentity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %s\n    key_env: %s\n", id.Name, id.KeyEnv)
	if id.MaxTier != nil {
		fmt.Fprintf(&b, "    max_tier: %d\n", *id.MaxTier)
	} else {
		fmt.Fprintf(&b, "    # max_tier: %d (default)\n", id.EffectiveMaxTier())
	}
	if id.Profile != "" {
		fmt.Fprintf(&b, "    profile: %s\n", id.Profile)
	}
	if len(id.AllowFrom) > 0 {
		fmt.Fprintf(&b, "    allow_from: [%s]\n", strings.Join(id.AllowFrom, ", "))
	}
	return b.String()
}

func apikeyRotate(args []string) int {
	identity, rest := splitIdentity(args)
	fs := flag.NewFlagSet("apikey rotate", flag.ContinueOnError)
	env := fs.String("env", "", "secret name (default: from --config, else FBMCP_KEY_<IDENTITY>)")
	cfgPath := fs.String("config", "", "fbmcp.yaml: take key_env and secrets.dir from it")
	dir := fs.String("secrets-dir", "", "the file source folder (default: secrets.dir from --config; none = keyring)")
	noShow := fs.Bool("no-show", false, "do not print the value")
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if identity == "" && fs.NArg() > 0 {
		identity = fs.Arg(0)
	}
	if identity == "" {
		fmt.Fprintln(os.Stderr, "apikey rotate: identity name required")
		return 2
	}
	name, d := *env, *dir
	if *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for _, id := range cfg.Identities {
			if id.Name == identity && name == "" {
				name = id.KeyEnv
			}
		}
		if name == "" {
			fmt.Fprintf(os.Stderr, "apikey rotate: identity %s is not in %s\n", identity, *cfgPath)
			return 1
		}
		if d == "" {
			d = cfg.Secrets.Dir
		}
	}
	if name == "" {
		name = defaultKeyEnv(identity)
	}
	if os.Getenv(name) != "" {
		// F2: env wins, so a new file or keyring value would do nothing.
		fmt.Fprintf(os.Stderr, "apikey rotate: env %s is set; env wins, so the rotation would have no effect. Change the env value and restart fbmcp, or remove the env var and use the file source\n", name)
		return 1
	}
	if svc := serviceInfo(*cfgPath); svc.env[name] {
		// The same F2 trap one level up: the unit/service environment.
		fmt.Fprintf(os.Stderr, "apikey rotate: the %s environment sets %s; it wins over the file, so the rotation would have no effect. Remove it there (EnvironmentFile / service Environment), restart fbmcp once, then rotate\n", svc.name, name)
		return 1
	}
	value, err := newKeyValue()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	where, err := storeKey(d, name, value)
	if err != nil {
		fmt.Fprintln(os.Stderr, "apikey rotate:", err)
		return 1
	}
	showKey(identity, value, *noShow, where)
	if d != "" {
		fmt.Println("the running kernel applies a file change in a few seconds; the old key stops working then")
	} else {
		fmt.Println("keyring: run fb_config_reload (or restart fbmcp) to apply; the old key works until then")
	}
	return 0
}

func apikeyList(args []string) int {
	explicit, args := takeConfigFlag(args)
	if len(args) > 0 {
		explicit = args[0]
	}
	cfg, cfgPath, err := loadConfig(explicit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	secrets.SetDir(cfg.Secrets.Dir)
	svcEnv := serviceInfo(cfgPath).env
	code := 0
	for _, id := range cfg.Identities {
		_, src, err := secrets.Lookup(id.KeyEnv)
		if svcEnv[id.KeyEnv] {
			// The service environment wins over this shell's view.
			src, err = "service-env", nil
		}
		extra := fmt.Sprintf("max_tier=%d", id.EffectiveMaxTier())
		if id.Profile != "" {
			extra += " profile=" + id.Profile
		}
		if len(id.AllowFrom) > 0 {
			extra += " allow_from=" + strings.Join(id.AllowFrom, ",")
		}
		if err != nil {
			fmt.Printf("FAIL %-20s %-28s %s: %v\n", id.Name, id.KeyEnv, extra, err)
			code = 1
			continue
		}
		fmt.Printf("ok   %-20s %-28s %s source=%s\n", id.Name, id.KeyEnv, extra, src)
	}
	if len(cfg.Identities) == 0 {
		fmt.Println("no identities in", cfgPath)
	}
	return code
}
