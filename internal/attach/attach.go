// Package attach implements ADR-005's daemon+thin-client fallback: the
// lock-holding kernel listens on 127.0.0.1 plus a token file under state.dir;
// a later stdio process (Claude Desktop's second MCP host) splices its
// stdin/stdout onto that socket instead of starting a second kernel.
package attach

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	addrFile = "attach.addr"
	tokFile  = "attach.token"
	magic    = "FBMCP1 "
)

// ErrNoPrimary means the lock is held but no attach endpoint is reachable
// (old binary, still starting, or crashed after lock).
var ErrNoPrimary = errors.New("no attach endpoint on the running instance")

// Start publishes a localhost attach listener for stateDir. onSession is
// invoked in a new goroutine per authenticated connection (the conn is past
// the token handshake; the callback owns Close).
func Start(stateDir string, onSession func(net.Conn)) (stop func(), err error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(stateDir, tokFile), token+"\n"); err != nil {
		ln.Close()
		return nil, err
	}
	if err := writeFile(filepath.Join(stateDir, addrFile), ln.Addr().String()+"\n"); err != nil {
		ln.Close()
		return nil, err
	}

	var sessions sync.WaitGroup
	var once sync.Once
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			sessions.Add(1)
			go func(c net.Conn) {
				defer sessions.Done()
				defer c.Close()
				if err := handshakeServer(c, token); err != nil {
					return
				}
				onSession(c)
			}(c)
		}
	}()

	return func() {
		once.Do(func() {
			ln.Close()
			sessions.Wait()
			_ = os.Remove(filepath.Join(stateDir, addrFile))
			_ = os.Remove(filepath.Join(stateDir, tokFile))
		})
	}, nil
}

// Dial connects to the lock-holder's attach endpoint, retrying until timeout
// so a second Claude spawn can wait out kernel startup.
func Dial(stateDir string, timeout time.Duration) (net.Conn, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		c, err := tryDial(stateDir)
		if err == nil {
			return c, nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	if last == nil {
		last = ErrNoPrimary
	}
	return nil, last
}

// RunProxy splices this process's stdin/stdout to the lock-holder. Used when
// Acquire fails and stdin is a pipe (MCP client), not an interactive console.
func RunProxy(stateDir string) error {
	c, err := Dial(stateDir, 15*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	return Splice(c, os.Stdin, os.Stdout)
}

// PipedStdin reports whether stdin looks like an MCP client pipe rather than
// an interactive terminal. Console double-starts keep the fail-fast lock error.
func PipedStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return true
	}
	return fi.Mode()&os.ModeCharDevice == 0
}

// Splice copies stdin→conn and conn→stdout until either side closes.
func Splice(conn io.ReadWriteCloser, stdin io.Reader, stdout io.Writer) error {
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, stdin)
		_ = closeWrite(conn)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(stdout, conn)
		errc <- err
	}()
	err := <-errc
	_ = conn.Close()
	<-errc
	if err == io.EOF {
		return nil
	}
	return err
}

func tryDial(stateDir string) (net.Conn, error) {
	addrb, err := os.ReadFile(filepath.Join(stateDir, addrFile))
	if err != nil {
		return nil, ErrNoPrimary
	}
	tokb, err := os.ReadFile(filepath.Join(stateDir, tokFile))
	if err != nil {
		return nil, ErrNoPrimary
	}
	addr := strings.TrimSpace(string(addrb))
	token := strings.TrimSpace(string(tokb))
	if addr == "" || token == "" {
		return nil, ErrNoPrimary
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	if err := handshakeClient(c, token); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func handshakeServer(c net.Conn, token string) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	line, err := readLine(c, 96)
	if err != nil {
		return err
	}
	want := magic + token
	if subtle.ConstantTimeCompare([]byte(line), []byte(want)) != 1 {
		return fmt.Errorf("attach: bad token")
	}
	return c.SetDeadline(time.Time{})
}

func readLine(c net.Conn, max int) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < max {
		n, err := c.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return strings.TrimSpace(string(b)), nil
			}
			if one[0] != '\r' {
				b = append(b, one[0])
			}
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("attach: handshake line too long")
}

func handshakeClient(c net.Conn, token string) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(c, "%s%s\n", magic, token); err != nil {
		return err
	}
	return c.SetDeadline(time.Time{})
}

func writeFile(path, body string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	_ = os.Remove(path)
	return os.Rename(tmp, path)
}

func closeWrite(c io.ReadWriteCloser) error {
	type closer interface{ CloseWrite() error }
	if cw, ok := c.(closer); ok {
		return cw.CloseWrite()
	}
	return nil
}
