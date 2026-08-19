package main

import (
	"testing"

	"github.com/aleks/fbmcp/internal/policy"
)

// Fuse #2 (enumeration): every advertised Tier ≥ 1 tool is in the registry
// with a non-zero tier; Tier 3 stays disabled-by-default. Execution without
// confirm is enforced by the gate; this test makes the registry the source
// of the enumeration so a new mutation tool cannot ship unlisted.
func TestFuse2RegistryEnumeration(t *testing.T) {
	seen := map[string]policy.ToolMeta{}
	for _, m := range toolMeta {
		if _, dup := seen[m.Name]; dup {
			t.Errorf("duplicate tool %s", m.Name)
		}
		seen[m.Name] = m
	}
	mustGated := []string{
		"fb_write", "fb_index_rebuild", "fb_index_drop", "fb_session_kill",
		"fb_user_create", "fb_user_drop", "fb_grant", "fb_revoke",
		"fb_backup_start", "fb_restore_replace", "fb_shutdown_window",
		"fb_config_set", "fb_trace_start", "fb_backup_nbackup",
		"fb_db_drop",
		"fb_schedule_create", "fb_schedule_delete", "fb_retention_run",
		"fb_service_start", "fb_service_stop", "fb_service_restart",
	}
	for _, name := range mustGated {
		m, ok := seen[name]
		if !ok {
			t.Errorf("gated tool %s missing from toolMeta", name)
			continue
		}
		if m.Tier < 1 {
			t.Errorf("%s advertised as tier %d (must be ≥ 1)", name, m.Tier)
		}
	}
	if seen["fb_db_drop"].Tier != 3 {
		t.Errorf("fb_db_drop must remain Tier 3 disabled stub")
	}
	if seen["fb_restore_replace"].Tier != 2 {
		t.Errorf("fb_restore_replace must be Tier 2")
	}
	if len(seen["fb_restore_replace"].Preconditions) == 0 {
		t.Error("fb_restore_replace missing fail-closed preconditions")
	}
}
