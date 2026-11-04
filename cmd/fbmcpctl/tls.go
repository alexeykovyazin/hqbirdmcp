package main

// fbmcpctl tls (phase 12 B1): a self-signed certificate for remote mode and
// the public-key pin the bridge checks (fbmcp bridge --pin).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	tlsCertName = "tls.crt"
	tlsKeyName  = "tls.key"
)

func cmdTLS(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: fbmcpctl tls selfsign --out DIR --dns NAMES --ip ADDRS [--grant ACCOUNT] [--days 825] [--force | --renew]\n       fbmcpctl tls show <cert.pem>")
		return 2
	}
	switch args[0] {
	case "selfsign":
		return cmdTLSSelfsign(args[1:])
	case "show":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: fbmcpctl tls show <cert.pem>")
			return 2
		}
		c, err := readCert(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		printCert(c)
		return 0
	}
	fmt.Fprintln(os.Stderr, "unknown tls op:", args[0])
	return 2
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdTLSSelfsign(args []string) int {
	fs := flag.NewFlagSet("tls selfsign", flag.ContinueOnError)
	out := fs.String("out", "", "folder for tls.crt and tls.key")
	dns := fs.String("dns", "", "DNS names for the certificate, comma separated")
	ips := fs.String("ip", "", "IP addresses for the certificate, comma separated")
	grant := fs.String("grant", "", `service account that gets read access to the key ("NT SERVICE\fbmcp" / firebird)`)
	days := fs.Int("days", 825, "validity in days")
	force := fs.Bool("force", false, "replace an existing pair with a NEW key (the pin changes)")
	renew := fs.Bool("renew", false, "new certificate for the EXISTING key (the pin stays)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || (*dns == "" && *ips == "") {
		fmt.Fprintln(os.Stderr, "tls selfsign: --out and at least one of --dns / --ip are required")
		return 2
	}
	if *force && *renew {
		fmt.Fprintln(os.Stderr, "tls selfsign: use --force or --renew, not both")
		return 2
	}
	if *days < 1 || *days > 3650 {
		fmt.Fprintln(os.Stderr, "tls selfsign: --days must be 1..3650")
		return 2
	}
	var ipList []net.IP
	for _, s := range splitList(*ips) {
		ip := net.ParseIP(s)
		if ip == nil {
			fmt.Fprintf(os.Stderr, "tls selfsign: bad --ip %q\n", s)
			return 2
		}
		ipList = append(ipList, ip)
	}
	certPath, keyPath := filepath.Join(*out, tlsCertName), filepath.Join(*out, tlsKeyName)
	c, err := selfsign(certPath, keyPath, splitList(*dns), ipList, *days, *force, *renew)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tls selfsign:", err)
		return 1
	}
	if *grant != "" {
		if err := grantKeyRead(keyPath, *grant); err != nil {
			fmt.Fprintf(os.Stderr, "tls selfsign: certificate written, but the key access for %s failed: %v\n", *grant, err)
			return 1
		}
		fmt.Printf("key access: %s can read %s\n", *grant, keyPath)
	} else if !*renew { // --renew does not touch the key file (its access stays)
		fmt.Println("warning: no --grant; the service account may not be able to read the key")
	}
	fmt.Printf("wrote %s and %s\n", certPath, keyPath)
	printCert(c)
	fmt.Printf("\nfbmcp.yaml:\ntls:\n  cert: %s\n  key: %s\n", certPath, keyPath)
	fmt.Printf("\nclient: fbmcp bridge --url https://<host>:<port>/mcp --key-env <NAME> --pin %s\n", spkiPin(c))
	return 0
}

// selfsign writes the pair. renew keeps the key in keyPath; force replaces
// both; without either an existing file is refused.
func selfsign(certPath, keyPath string, dns []string, ips []net.IP, days int, force, renew bool) (*x509.Certificate, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	exists := certErr == nil || keyErr == nil
	if exists && !force && !renew {
		return nil, fmt.Errorf("%s or %s exists; use --renew (keep the key and the pin) or --force (new key, new pin)", certPath, keyPath)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o750); err != nil {
		return nil, err
	}
	var key *ecdsa.PrivateKey
	if renew {
		k, err := readECKey(keyPath)
		if err != nil {
			return nil, fmt.Errorf("--renew needs the existing key: %w", err)
		}
		key = k
	} else {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		key = k
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	cn := "fbmcp"
	if len(dns) > 0 {
		cn = dns[0]
	} else if len(ips) > 0 {
		cn = ips[0].String()
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"fbmcp self-signed"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(time.Duration(days) * 24 * time.Hour),
		DNSNames:              dns,
		IPAddresses:           ips,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-signed: clients trust it as its own CA (--ca / NODE_EXTRA_CA_CERTS)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	if !renew {
		kb, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
			return nil, err
		}
	}
	if err := writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// writeAtomic writes via a temp file + rename, so the kernel's cert reload
// (B0.6) never sees half a file.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	if err := keepOwner(name, path); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func readECKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	if k, err := x509.ParsePKCS8PrivateKey(blk.Bytes); err == nil {
		if ec, ok := k.(*ecdsa.PrivateKey); ok {
			return ec, nil
		}
		return nil, fmt.Errorf("%s: not an ECDSA key (--renew supports keys made by tls selfsign)", path)
	}
	if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	return nil, fmt.Errorf("%s: cannot parse the key", path)
}

func readCert(path string) (*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New(path + ": no PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// spkiPin is sha256 over SubjectPublicKeyInfo: a renewal with the same key
// keeps it (B1), and it equals the openssl pipeline in the runbook.
func spkiPin(c *x509.Certificate) string {
	h := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(h[:])
}

func printCert(c *x509.Certificate) {
	var sans []string
	sans = append(sans, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		sans = append(sans, ip.String())
	}
	left := int(time.Until(c.NotAfter).Hours() / 24)
	fmt.Printf("subject:  %s\n", c.Subject.CommonName)
	fmt.Printf("names:    %s\n", strings.Join(sans, ", "))
	fmt.Printf("expires:  %s (%d days)\n", c.NotAfter.UTC().Format("2006-01-02"), left)
	fmt.Printf("pin:      %s\n", spkiPin(c))
	if left < 30 {
		fmt.Println("warning: expires in less than 30 days; run tls selfsign --renew")
	}
}
