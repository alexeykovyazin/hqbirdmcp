package main

import (
	"context"
	"time"

	"github.com/aleks/fbmcp/internal/audit"
	"github.com/aleks/fbmcp/internal/config"
	"github.com/aleks/fbmcp/internal/identity"
	"github.com/aleks/fbmcp/internal/reload"
)

func (gt *gatedTools) reloadHooks() reload.Hooks {
	return reload.Hooks{
		HasLiveBackup: func(dbID string) bool {
			for _, j := range gt.st.Jobs() {
				if j.Database == dbID && (j.State == "queued" || j.State == "running") {
					switch j.Type {
					case "fb_backup_start", "fb_restore_test", "fb_restore_replace", "nightly_verify":
						return true
					}
				}
			}
			return false
		},
		HasLiveServiceJob: func(instanceID string) bool {
			for _, j := range gt.st.Jobs() {
				if j.State != "queued" && j.State != "running" {
					continue
				}
				if j.Type != "fb_service_start" && j.Type != "fb_service_stop" && j.Type != "fb_service_restart" {
					continue
				}
				if j.Database == instanceID {
					return true
				}
				db, err := gt.cfg.DB(j.Database)
				if err == nil && db.Instance == instanceID {
					return true
				}
			}
			return false
		},
		SetDraining: gt.runner.SetDraining,
		StopTraces: func(ids []string) {
			want := map[string]bool{}
			for _, id := range ids {
				want[id] = true
			}
			byID := map[string]string{}
			for _, tr := range gt.st.Traces() {
				byID[tr.ID] = tr.Database
			}
			gt.mu.Lock()
			defer gt.mu.Unlock()
			for tid, lt := range gt.traces {
				if !want[byID[tid]] {
					continue
				}
				_ = lt.Stop()
				delete(gt.traces, tid)
				_ = gt.st.RemoveTrace(tid)
			}
		},
		DropPending: func(dbIDs, dropInstanceIDs []string) {
			dropped := gt.st.DropPendingFor(dbIDs, dropInstanceIDs)
			gt.mu.Lock()
			for _, id := range dropped {
				delete(gt.args, id)
			}
			gt.mu.Unlock()
		},
		RemapState: func(oldID, newID string) {
			_ = gt.st.RemapDatabaseID(oldID, newID)
		},
		WaitIdle: func(ctx context.Context, id, exceptJobID string) error {
			if err := gt.runner.WaitIdle(ctx, id, exceptJobID); err != nil {
				return err
			}
			for {
				busy := false
				for _, w := range gt.st.RunningWorkflows() {
					if w.Database == id {
						busy = true
						break
					}
				}
				if !busy {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(50 * time.Millisecond):
				}
			}
		},
		CloseDB: func(ids []string) {
			for _, id := range ids {
				gt.pools.CloseDB(id)
			}
		},
		InvalidateFacts: func(ids []string) {
			if gt.facts != nil {
				gt.facts.Invalidate(ids...)
			}
		},
		SetWebhook: func(url, secretEnv string) error {
			secret := ""
			if secretEnv != "" {
				secret, _ = config.SecretFromEnv(secretEnv)
			}
			if gt.bus != nil {
				gt.bus.SetWebhook(url, secret)
			}
			return nil
		},
		ApplyAuth: func(cfg *config.Config) error {
			identity.SetLocalMaxTier(cfg.LocalMaxTierOrDefault())
			if gt.httpLn == nil {
				return nil
			}
			return gt.httpLn.ReplaceAuth(cfg)
		},
		ApplyListener: func(old, new *config.Config) error {
			if gt.httpLn == nil {
				return nil
			}
			return gt.httpLn.Replace(old, new)
		},
		Audit: gt.auditReload,
	}
}

func formatReload(res reload.Result, err error) string {
	if err != nil {
		return "reload denied: " + err.Error()
	}
	if res.Class == "noop" {
		return "reload: no changes"
	}
	if res.Message != "" {
		return res.Message
	}
	return "reload applied"
}

func (gt *gatedTools) auditReload(res reload.Result, err error) {
	if gt == nil || gt.aud == nil {
		return
	}
	dec := res.Class
	if err != nil {
		dec = "denied"
	}
	detail := map[string]interface{}{"message": res.Message, "reason": res.Reason, "hash": res.Hash}
	if err != nil {
		detail["error"] = err.Error()
	}
	tool := res.Reason
	if tool == "" {
		tool = "fb_config_reload"
	}
	gt.aud.Log(audit.Entry{Identity: "local", Tool: tool, Tier: 0, Decision: dec, Detail: detail})
}
