package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
)

// Handle is a process-lifetime holder of an immutable Config snapshot.
// Reload swaps the pointer; readers always call Current() / DB() / Instance().
type Handle struct {
	ptr  atomic.Pointer[Config]
	hash atomic.Value // string
}

func NewHandle(c *Config) *Handle {
	h := &Handle{}
	h.Swap(c)
	return h
}

func (h *Handle) Current() *Config {
	if h == nil {
		return nil
	}
	return h.ptr.Load()
}

func (h *Handle) Hash() string {
	if h == nil {
		return ""
	}
	v, _ := h.hash.Load().(string)
	return v
}

func (h *Handle) Swap(c *Config) {
	h.ptr.Store(c)
	h.hash.Store(SnapshotHash(c))
}

func (h *Handle) DB(id string) (Database, error) {
	return h.Current().DB(id)
}

func (h *Handle) Instance(id string) (FBInstance, error) {
	return h.Current().Instance(id)
}

// SnapshotHash is a canonical digest of loaded config (SourcePath ignored;
// nil slices treated as empty). Used to no-op watcher events after Apply.
func SnapshotHash(c *Config) string {
	if c == nil {
		return ""
	}
	n := canonical(c)
	b, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func canonical(c *Config) Config {
	out := *c
	out.SourcePath = ""
	if out.Instances == nil {
		out.Instances = []FBInstance{}
	}
	if out.Databases == nil {
		out.Databases = []Database{}
	}
	if out.Identities == nil {
		out.Identities = []APIIdentity{}
	}
	return out
}
