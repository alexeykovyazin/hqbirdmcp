package policy

// SystemTools is the frozen fbmcp tool surface (single source of truth for
// the kernel's toolMeta, the D6.2 drift check, and fbmcpctl gate's live
// Appendix A statement). Keep in sync ONLY by editing here — cmd/fbmcp
// aliases it. Preconditions are part of the surface definition.
func SystemTools() []ToolMeta {
	return []ToolMeta{
		{Name: "fb_ping", Tier: 0, Scope: "database"},
		{Name: "fb_db_list", Tier: 0, Scope: "database"},
		{Name: "fb_db_health", Tier: 0, Scope: "database"},
		{Name: "fb_job_status", Tier: 0, Scope: "database"},
		{Name: "fb_confirm", Tier: 0, Scope: "database"}, // gate entry point, not a mutation
		{Name: "fb_cancel", Tier: 0, Scope: "database"},
		{Name: "fb_demo_write", Tier: 1, Scope: "database", RetrySafe: true},
		{Name: "fb_info", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_connected_dbs", Tier: 0, Scope: "instance", MinFB: "2.5"},
		{Name: "fb_db_register", Tier: 2, Scope: "instance"},
		{Name: "fb_config_reload", Tier: 0, Scope: "instance"},
		{Name: "fb_sessions", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_transactions", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_analyze_query", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_index_advice", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.2: plan analysis → proposed CREATE INDEX DDL (apply via fb_write)
		{Name: "fb_index_stats", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_gstat", Tier: 0, Scope: "database"}, // ADR-003 gstat route; no MinFB — utility route, works without a server
		{Name: "fb_schema_list", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_describe", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_diff_schema", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.3: two dbs or snapshot-vs-now
		{Name: "fb_diff_data", Tier: 0, Scope: "database", MinFB: "2.5"},   // C.3: bounded key-based data diff
		{Name: "fb_activity_sample", Tier: 0, Scope: "database", MinFB: "2.5"},
		{Name: "fb_trends", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.4: sampler history → capacity projections
		{Name: "fb_lwmonitoring", Tier: 0, Scope: "instance"},
		{Name: "fb_backup_start", Tier: 1, Scope: "database"},
		{Name: "fb_restore_test", Tier: 1, Scope: "database"},
		{Name: "fb_validate", Tier: 1, Scope: "database"},
		{Name: "fb_sweep", Tier: 1, Scope: "database"},
		{Name: "fb_set_forcewrite", Tier: 1, Scope: "database"},
		{Name: "fb_set_readonly", Tier: 1, Scope: "database"},
		{Name: "fb_service_status", Tier: 0, Scope: "instance"},
		{Name: "fb_write", Tier: 1, Scope: "database"},                                 // dynamic tier: classified per request
		{Name: "fb_migration_status", Tier: 0, Scope: "database", MinFB: "2.5"},        // C.1: dir vs history
		{Name: "fb_migration_plan", Tier: 0, Scope: "database", MinFB: "2.5"},          // C.1: dry-run classification
		{Name: "fb_migration_apply", Tier: 1, Scope: "database"},                       // C.1: ADR-030 batch gate; dynamic tier per batch
		{Name: "fb_migration_rollback_plan", Tier: 0, Scope: "database", MinFB: "2.5"}, // C.1: renders recorded down sections
		{Name: "fb_query", Tier: 0, Scope: "database", MinFB: "2.5"},                   // read-only tx; fallback into fb_write's gated flow for refused EXECUTE PROCEDURE
		{Name: "fb_index_rebuild", Tier: 1, Scope: "database"},
		{Name: "fb_index_drop", Tier: 1, Scope: "database"},
		{Name: "fb_session_kill", Tier: 1, Scope: "database"},
		{Name: "fb_user_create", Tier: 1, Scope: "database"},
		{Name: "fb_user_drop", Tier: 1, Scope: "database"},
		{Name: "fb_role_create", Tier: 1, Scope: "database"},
		{Name: "fb_grant", Tier: 1, Scope: "database"},
		{Name: "fb_revoke", Tier: 1, Scope: "database"},
		{Name: "fb_comment_set", Tier: 1, Scope: "database"},
		{Name: "fb_db_create", Tier: 1, Scope: "database"},
		{Name: "fb_db_drop", Tier: 3, Scope: "database"},
		{Name: "fb_shutdown_window", Tier: 2, Scope: "database", Preconditions: []Precondition{
			{Name: "verified_backup_exists", Op: "true", Why: "verified backup required"},
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "backup < 24h"},
		}},
		{Name: "fb_config_get", Tier: 0, Scope: "instance"},
		{Name: "fb_config_diff", Tier: 0, Scope: "instance"},
		{Name: "fb_config_set", Tier: 2, Scope: "instance"},
		{Name: "fb_window_open", Tier: 1, Scope: "database"},
		{Name: "fb_set_page_buffers", Tier: 1, Scope: "database"},
		{Name: "fb_restore_replace", Tier: 2, Scope: "database", Preconditions: []Precondition{
			{Name: "verified_backup_exists", Op: "true", Why: "verified backup required"},
			{Name: "backup_freshness", Op: "lt", Value: 24.0, Why: "backup < 24h"},
		}},
		{Name: "fb_backup_nbackup", Tier: 1, Scope: "database"},
		{Name: "fb_trace_start", Tier: 1, Scope: "database"},
		{Name: "fb_trace_stop", Tier: 1, Scope: "database"},
		{Name: "fb_trace_list", Tier: 0, Scope: "database"},
		{Name: "fb_effective_access", Tier: 0, Scope: "database"},
		{Name: "fb_schedule_list", Tier: 0, Scope: "database"},
		{Name: "fb_schedule_create", Tier: 1, Scope: "database"}, // dynamic: max tier of target
		{Name: "fb_schedule_delete", Tier: 1, Scope: "database"},
		{Name: "fb_retention_run", Tier: 1, Scope: "database"},
		{Name: "fb_service_start", Tier: 2, Scope: "instance"},
		{Name: "fb_service_stop", Tier: 2, Scope: "instance"},
		{Name: "fb_service_restart", Tier: 2, Scope: "instance"},
	}
}
