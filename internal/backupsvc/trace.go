package backupsvc

import (
	"context"
	"fmt"
	"io"

	fb "github.com/nakagami/firebirdsql"
)

const TraceCap = 8 << 20 // 8 MiB server-side drain cap (ADR-018)

// TraceTemplates is the ADR-018 allowlist. Keys are the only accepted
// `template` argument values.
var TraceTemplates = map[string]string{
	"audit-lite": `database {
  enabled = true
  log_connections = true
  log_statement_finish = true
  max_log_size = 8
}
`,
	"performance": `database {
  enabled = true
  log_statement_finish = true
  time_threshold = 100
  max_log_size = 8
}
`,
	"errors": `database {
  enabled = true
  log_errors = true
  max_log_size = 8
}
`,
}

// LiveTrace is an in-process engine session plus its drain.
type LiveTrace struct {
	Name   string
	Sess   *fb.TraceSession
	cancel context.CancelFunc
}

// StartTrace starts a named template session and drains output into w
// until cap or cancel. The caller must keep LiveTrace until Stop.
func (c *Client) StartTrace(ctx context.Context, name, template string, w io.Writer) (*LiveTrace, error) {
	cfg, ok := TraceTemplates[template]
	if !ok {
		return nil, fmt.Errorf("unknown trace template %q (allowed: audit-lite, performance, errors)", template)
	}
	tm, err := fb.NewTraceManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return nil, err
	}
	sess, err := tm.StartWithName(name, cfg)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithCancel(ctx)
	lt := &LiveTrace{Name: name, Sess: sess, cancel: cancel}
	ch := make(chan string, 256)
	go func() { _ = sess.WaitStrings(ch) }()
	go func() {
		var written int64
		for {
			select {
			case <-dctx.Done():
				return
			case line, ok := <-ch:
				if !ok {
					return
				}
				if written >= TraceCap {
					_ = sess.Stop()
					return
				}
				n, _ := io.WriteString(w, line+"\n")
				written += int64(n)
			}
		}
	}()
	return lt, nil
}

func (lt *LiveTrace) Stop() error {
	if lt == nil {
		return nil
	}
	if lt.cancel != nil {
		lt.cancel()
	}
	if lt.Sess != nil {
		return lt.Sess.Close()
	}
	return nil
}

// ListTrace returns the engine's session list (best-effort).
func (c *Client) ListTrace() (string, error) {
	tm, err := fb.NewTraceManager(c.Addr, c.User, c.Pass, c.opts)
	if err != nil {
		return "", err
	}
	return tm.List()
}
