// Package notify is K7: local event log + optional HMAC-signed webhook
// (ADR-024). The fire path never talks to the gate.
package notify

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Event is one notification.
type Event struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Database string            `json:"database,omitempty"`
	Tool     string            `json:"tool,omitempty"`
	Message  string            `json:"message"`
	At       time.Time         `json:"at"`
	Detail   map[string]string `json:"detail,omitempty"`
}

// Bus delivers events to the local log and optional webhook.
type Bus struct {
	mu            sync.Mutex
	logPath       string
	webhookURL    string
	webhookSecret string
	client        *http.Client
	failures      int
	deliver       func(url string, body []byte, id, sig string) error // test hook
}

func New(stateDir, webhookURL, webhookSecret string) (*Bus, error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	b := &Bus{
		logPath:       filepath.Join(stateDir, "events.jsonl"),
		webhookURL:    webhookURL,
		webhookSecret: webhookSecret,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
	b.deliver = b.httpDeliver
	return b, nil
}

func (b *Bus) Failures() int { b.mu.Lock(); defer b.mu.Unlock(); return b.failures }

// SetWebhook updates delivery target without reopening the local event log.
func (b *Bus) SetWebhook(url, secret string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.webhookURL = url
	b.webhookSecret = secret
}

func (b *Bus) Emit(e Event) error {
	if e.ID == "" {
		e.ID = newID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := b.appendLog(append(body, '\n')); err != nil {
		return err
	}
	b.mu.Lock()
	url, secret := b.webhookURL, b.webhookSecret
	b.mu.Unlock()
	if url == "" || secret == "" {
		return nil
	}
	sig := Sign(secret, body)
	var last error
	backoff := 20 * time.Millisecond
	for i := 0; i < 3; i++ {
		last = b.deliver(url, body, e.ID, sig)
		if last == nil {
			return nil
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	b.mu.Lock()
	b.failures++
	b.mu.Unlock()
	return last
}

func (b *Bus) appendLog(line []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	f, err := os.OpenFile(b.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

func (b *Bus) httpDeliver(url string, body []byte, id, sig string) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-FBMCP-Event-Id", id)
	req.Header.Set("X-FBMCP-Signature", "sha256="+sig)
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook status %d", resp.StatusCode)
	}
	return nil
}

// Sign returns hex HMAC-SHA256 of body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify checks HMAC-SHA256 (constant-time).
func Verify(secret, sigHex string, body []byte) bool {
	want, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(want, m.Sum(nil))
}

// ReplayGuard rejects duplicate event IDs (test / local receiver).
type ReplayGuard struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

func NewReplayGuard() *ReplayGuard { return &ReplayGuard{seen: map[string]struct{}{}} }

func (g *ReplayGuard) Accept(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.seen[id]; ok {
		return false
	}
	g.seen[id] = struct{}{}
	return true
}

func newID() string {
	return fmt.Sprintf("e%d", time.Now().UnixNano())
}
