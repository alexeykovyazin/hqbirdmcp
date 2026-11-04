package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"
)

// Log file limits (phase 12 B0.10): 10 MiB per file, 5 rotated copies.
const (
	logMaxBytes = 10 << 20
	logKeep     = 5
)

// teeStderrToFile copies everything written to os.Stderr (and the log
// package) to <dir>/fbmcp.log as well, with a time stamp per line. A Windows
// service has no usable stderr, so without this its errors are lost (F17).
// Fatal crash output goes to <dir>/fbmcp-crash.log (the runtime writes it
// straight to fd 2, around this tee).
func teeStderrToFile(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	rf, err := openRotating(filepath.Join(dir, "fbmcp.log"), logMaxBytes, logKeep)
	if err != nil {
		return nil, err
	}
	if cf, err := os.OpenFile(filepath.Join(dir, "fbmcp-crash.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640); err == nil {
		_ = debug.SetCrashOutput(cf, debug.CrashOptions{})
		cf.Close() // SetCrashOutput keeps its own duplicate
	}
	r, w, err := os.Pipe()
	if err != nil {
		rf.Close()
		return nil, err
	}
	orig := os.Stderr
	os.Stderr = w
	log.SetOutput(w)
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				_, _ = orig.Write(line) // journal / console; fails silently under the SCM
				_, _ = rf.Write(append([]byte(time.Now().UTC().Format(time.RFC3339)+" "), line...))
			}
			if err != nil {
				return
			}
		}
	}()
	fmt.Fprintf(os.Stderr, "fbmcp: start pid %d, log %s\n", os.Getpid(), rf.path)
	return func() {
		os.Stderr = orig
		log.SetOutput(orig)
		w.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		r.Close()
		rf.Close()
	}, nil
}

// rotatingFile is a size-rotated append-only file: path, path.1 … path.N.
type rotatingFile struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
}

func openRotating(path string, max int64, keep int) (*rotatingFile, error) {
	r := &rotatingFile{path: path, max: max, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, fi.Size()
	return nil
}

func (r *rotatingFile) rotate() {
	r.f.Close()
	os.Remove(fmt.Sprintf("%s.%d", r.path, r.keep))
	for i := r.keep - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	os.Rename(r.path, r.path+".1")
	if err := r.open(); err != nil {
		r.f = nil
	}
}

func (r *rotatingFile) Write(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(b)) > r.max && r.size > 0 {
		r.rotate()
		if r.f == nil {
			return 0, fmt.Errorf("log rotate failed")
		}
	}
	n, err := r.f.Write(b)
	r.size += int64(n)
	return n, err
}

func (r *rotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// flushLog drains the stderr tee (set in serve mode). die calls it before
// os.Exit, so a start-up error reaches the journal / fbmcp.log instead of
// being lost in the pipe (L1 finding: busy port left no line).
var flushLog = func() {}

// die prints a fatal error, flushes the log tee and exits 1.
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	flushLog()
	os.Exit(1)
}
