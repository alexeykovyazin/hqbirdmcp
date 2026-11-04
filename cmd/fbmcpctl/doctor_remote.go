package main

// fbmcpctl doctor --remote <config> (phase 12 B6): one line per check of a
// remote-mode install. Values of keys are never printed (F18).

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/secrets"
	"github.com/aleks/fbmcp/internal/transport"
)

type doctorOut struct {
	fails, warns int
}

func (d *doctorOut) ok(name, f string, a ...any) {
	fmt.Printf("ok   %-12s %s\n", name, fmt.Sprintf(f, a...))
}

func (d *doctorOut) warn(name, f string, a ...any) {
	d.warns++
	fmt.Printf("WARN %-12s %s\n", name, fmt.Sprintf(f, a...))
}

func (d *doctorOut) fail(name, f string, a ...any) {
	d.fails++
	fmt.Printf("FAIL %-12s %s\n", name, fmt.Sprintf(f, a...))
}

func doctorRemote(cfgPath string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	secrets.SetDir(cfg.Secrets.Dir)
	d := &doctorOut{}
	fmt.Printf("doctor --remote %s (server_name %s)\n", cfgPath, cfg.ServerNameOrDefault())

	if err := transport.CheckRemote(cfg.Listen, cfg.TLS.Cert, cfg.TLS.Key, len(cfg.Identities), len(cfg.AllowedOrigins)); err != nil {
		d.fail("config", "%v", err)
	} else if strings.TrimSpace(cfg.Listen) == "" {
		d.fail("config", "listen is empty: this is not a remote-mode config")
	} else {
		d.ok("config", "listen %s, %d identities, %d origins", cfg.Listen, len(cfg.Identities), len(cfg.AllowedOrigins))
	}
	svc := serviceInfo(cfgPath)
	if svc.err != "" {
		d.warn("service", "%s", svc.err)
	} else {
		if !svc.serve && svc.serveImplied {
			d.warn("service", "%s has no -serve flag: a Windows service serves anyway, but install-remote.ps1 adds it: %s", svc.name, svc.cmd)
		} else if !svc.serve {
			d.fail("service", "the service command has no -serve flag (it exits when stdin closes): %s", svc.cmd)
		} else {
			d.ok("service", "%s runs as %s with -serve", svc.name, svc.account)
		}
	}

	var pin string
	if cfg.TLS.Cert != "" {
		pin = checkTLS(d, cfg, svc)
	}
	checkIdentities(d, cfg, svc.env)
	if strings.TrimSpace(cfg.Listen) != "" {
		checkListener(d, cfg, pin)
		checkFirewall(d, cfg)
	}
	checkPlatform(d, cfg, svc)

	fmt.Printf("doctor --remote: %d FAIL, %d WARN\n", d.fails, d.warns)
	if d.fails > 0 {
		return 1
	}
	return 0
}

func checkTLS(d *doctorOut, cfg *config.Config, svc svcInfo) string {
	pair, err := tls.LoadX509KeyPair(cfg.TLS.Cert, cfg.TLS.Key)
	if err != nil {
		d.fail("tls", "cannot load the pair: %v", err)
		return ""
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		d.fail("tls", "parse cert: %v", err)
		return ""
	}
	pin := spkiPin(leaf)
	left := time.Until(leaf.NotAfter)
	switch {
	case left <= 0:
		d.fail("tls", "certificate expired on %s", leaf.NotAfter.UTC().Format("2006-01-02"))
	case left < 30*24*time.Hour:
		d.warn("tls", "certificate expires in %d days (tls selfsign --renew keeps the pin)", int(left.Hours()/24))
	default:
		d.ok("tls", "expires %s, pin %s", leaf.NotAfter.UTC().Format("2006-01-02"), pin)
	}
	if host, _, err := net.SplitHostPort(cfg.Listen); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		if leaf.VerifyHostname(host) != nil {
			d.warn("tls", "the certificate has no name for the listen host %s (clients with --ca must use a name from the cert; --pin is not affected)", host)
		}
	}
	if svc.account != "" {
		if lvl, msg := keyAccess(cfg.TLS.Key, svc.account); lvl == "ok" {
			d.ok("tls-key", "%s", msg)
		} else if lvl == "WARN" {
			d.warn("tls-key", "%s", msg)
		} else {
			d.fail("tls-key", "%s", msg)
		}
	}
	return pin
}

func checkIdentities(d *doctorOut, cfg *config.Config, svcEnv map[string]bool) {
	for _, id := range cfg.Identities {
		_, src, err := secrets.Lookup(id.KeyEnv)
		label := "key:" + id.Name
		switch {
		case svcEnv[id.KeyEnv]:
			// The service gets it from its own environment (unit
			// EnvironmentFile / service registry Environment), not this shell.
			d.warn(label, "%s is set in the service environment: it wins over the secrets dir, so a rotation needs a restart; move it to the secrets dir", id.KeyEnv)
		case err != nil:
			d.fail(label, "%s does not resolve: %v", id.KeyEnv, err)
		case src == secrets.SourceEnv:
			d.warn(label, "%s comes from env (in this shell); a rotation then needs a restart, prefer the secrets dir", id.KeyEnv)
		case src == secrets.SourceKeyring:
			d.warn(label, "%s comes from the keyring of the CURRENT account; the service account has its own (or none) — prefer the secrets dir", id.KeyEnv)
		default:
			d.ok(label, "%s source=%s", id.KeyEnv, src)
		}
		if id.Autonomous() {
			if id.EffectiveMaxTier() >= 1 {
				d.warn(label, "autonomous identity with max_tier %d: it can request writes (B8; every request still needs an operator)", id.EffectiveMaxTier())
			}
			if len(id.AllowFrom) == 0 {
				d.warn(label, "autonomous identity without allow_from: the key works from any address (B8)")
			}
		}
	}
}

// envNames keeps the names of NAME=value lines (systemd EnvironmentFile
// syntax, service registry Environment). Values are dropped at once.
func envNames(lines []string) map[string]bool {
	names := map[string]bool{}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		if k, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) != "" {
			names[strings.TrimSpace(k)] = true
		}
	}
	return names
}

// checkListener: the port answers, it belongs to fbmcp, /healthz says ok
// with the configured certificate.
func checkListener(d *doctorOut, cfg *config.Config, pin string) {
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		d.fail("listen", "%v", err)
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, port)
	// A TLS handshake (no verification here; /healthz below verifies), so
	// the server log gets no "TLS handshake error ... EOF" line.
	c, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		d.fail("listen", "no TLS listener on %s: %v (service down? see the log)", addr, err)
		return
	}
	c.Close()
	if owner := portOwner(port); owner == "" {
		d.ok("listen", "%s answers", addr)
	} else if strings.Contains(strings.ToLower(owner), "fbmcp") {
		d.ok("listen", "%s answers (process %s)", addr, owner)
	} else {
		d.fail("listen", "%s is held by %s, not fbmcp", addr, owner)
	}
	var served string
	tc := &tls.Config{InsecureSkipVerify: true, // the pin compare below is the check
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				h := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
				served = "sha256:" + hex.EncodeToString(h[:])
			}
			return nil
		}}
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: tc}}
	resp, err := hc.Get("https://" + addr + "/healthz")
	if err != nil {
		d.fail("healthz", "%v", err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.TrimSpace(string(body)) != "ok" {
		d.fail("healthz", "HTTP %d %q", resp.StatusCode, strings.TrimSpace(string(body)))
		return
	}
	if pin != "" && served != pin {
		d.warn("healthz", "ok, but the server uses another key (%s, config has %s): reload pending or wrong files", served, pin)
		return
	}
	d.ok("healthz", "https://%s/healthz = ok", addr)
}

func checkFirewall(d *doctorOut, cfg *config.Config) {
	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return
	}
	lvl, msg := firewallState(port)
	switch lvl {
	case "ok":
		d.ok("firewall", "%s", msg)
	case "FAIL":
		d.fail("firewall", "%s", msg)
	default:
		d.warn("firewall", "%s", msg)
	}
}
