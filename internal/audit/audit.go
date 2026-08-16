// Package audit implements the P1.4 append-only, hash-chained audit log
// (JSONL) with secret scrubbing and integrity verification.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry is one audit record. Chain: each entry hashes the previous hash.
type Entry struct {
	Time    time.Time              `json:"time"`
	Identity string                `json:"identity"`
	Database string                `json:"database,omitempty"`
	Tool    string                 `json:"tool"`
	Tier    int                    `json:"tier"`
	Decision string                `json:"decision"` // allow | pending | approved | denied | error
	Channel string                 `json:"channel,omitempty"` // elicitation | in-band-token | out-of-band | —
	Detail  map[string]interface{} `json:"detail,omitempty"`
	PrevHash string                `json:"prev_hash"`
	Hash     string                `json:"hash"`
}

// scrubbed keys never appear in Detail values (defense against credential
// leakage into the audit trail — threat T-06).
var secretMarkers = []string{"password", "passwd", "secret", "token", "isc_password"}

// Logger writes hash-chained entries to <dir>/audit.jsonl.
type Logger struct {
	mu    sync.Mutex
	path  string
	last  string // last hash
	count int
	f     *os.File
}

func Open(dir string) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "audit.jsonl")
	l := &Logger{path: p}
	// resume chain from existing file
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		var e Entry
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err == nil {
			l.last = e.Hash
			l.count = len(lines)
		}
	}
	// cross-check against head sidecar (truncation detection)
	if hb, err := os.ReadFile(p + ".head"); err == nil {
		var head struct {
			Count int    `json:"count"`
			Last  string `json:"last"`
		}
		if json.Unmarshal(hb, &head) == nil {
			if head.Count != l.count || head.Last != l.last {
				return nil, fmt.Errorf("audit: head sidecar mismatch (log truncated or forged?): file has count=%d last=%s, head says count=%d last=%s",
					l.count, shortHash(l.last), head.Count, shortHash(head.Last))
			}
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

func (l *Logger) Log(e Entry) error {
	e.Time = time.Now().UTC()
	scrub(e.Detail)
	e.PrevHash = l.last
	if e.PrevHash == "" {
		e.PrevHash = strings.Repeat("0", 64)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	e.Hash = hash(string(b))
	b, err = json.Marshal(e)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.last = e.Hash
	l.count++
	return os.WriteFile(l.path+".head", []byte(fmt.Sprintf(`{"count":%d,"last":%q}`+"\n", l.count, l.last)), 0o640)
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Verify checks the whole chain; returns the first broken line number (0 = OK).
func Verify(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	prev := strings.Repeat("0", 64)
	line := 0
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line++
		if l == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			return line, fmt.Errorf("line %d: unparseable: %w", line, err)
		}
		if e.PrevHash != prev {
			return line, fmt.Errorf("line %d: chain break (prev_hash mismatch)", line)
		}
		storedHash := e.Hash
		e.Hash = ""
		rb, err := json.Marshal(e)
		if err != nil {
			return line, err
		}
		if hash(string(rb)) != storedHash {
			return line, fmt.Errorf("line %d: hash mismatch (tampered)", line)
		}
		prev = storedHash
	}
	// truncation check against head sidecar
	if hb, err := os.ReadFile(path + ".head"); err == nil {
		var head struct {
			Count int    `json:"count"`
			Last  string `json:"last"`
		}
		if json.Unmarshal(hb, &head) == nil {
			if head.Count != line-1 && head.Count != line { // tolerate trailing newline counting variance
				return line, fmt.Errorf("truncated: log has %d entries, head records %d", line-1, head.Count)
			}
			if head.Last != prev {
				return line, fmt.Errorf("truncated: last hash does not match head")
			}
		}
	}
	return 0, nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

func scrub(d map[string]interface{}) {
	for k, v := range d {
		lk := strings.ToLower(k)
		dropped := false
		for _, m := range secretMarkers {
			if strings.Contains(lk, m) {
				delete(d, k)
				dropped = true
				break
			}
		}
		if dropped {
			continue
		}
		if s, ok := v.(string); ok {
			d[k] = scrubString(s)
		}
	}
}

func scrubString(s string) string {
	for _, m := range secretMarkers {
		if i := strings.Index(strings.ToLower(s), m); i >= 0 {
			return s[:i] + "[redacted]"
		}
	}
	return s
}
