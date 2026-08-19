package config

import (
	"fmt"
	"strings"

	"github.com/aleks/fbmcp/internal/configedit"
	"gopkg.in/yaml.v3"
)

type RegisterOptions struct {
	InstanceID     string
	DBID           string
	Path           string
	BackupDir      string
	WorkDir        string
	ROUser         string
	ROSecretEnv    string
	AdminUser      string
	AdminSecretEnv string
}

func MaterializeDatabase(cfg *Config, opt RegisterOptions) (Database, error) {
	inst, err := cfg.Instance(opt.InstanceID)
	if err != nil {
		return Database{}, err
	}
	if err := inst.ValidateRegistrationDefaults(); err != nil {
		return Database{}, err
	}
	db := Database{
		ID:             opt.DBID,
		Instance:       inst.ID,
		Path:           opt.Path,
		BackupDir:      firstNonEmpty(opt.BackupDir, inst.DefaultBackupDir),
		WorkDir:        firstNonEmpty(opt.WorkDir, inst.DefaultWorkDir),
		ROUser:         firstNonEmpty(opt.ROUser, inst.DefaultROUser),
		ROSecretEnv:    firstNonEmpty(opt.ROSecretEnv, inst.DefaultROSecretEnv),
		AdminUser:      firstNonEmpty(opt.AdminUser, inst.DefaultAdminUser),
		AdminSecretEnv: firstNonEmpty(opt.AdminSecretEnv, inst.DefaultAdminSecretEnv),
	}
	if !idRe.MatchString(db.ID) {
		return Database{}, fmt.Errorf("database id %q: must match %s", db.ID, idRe)
	}
	if strings.TrimSpace(db.Path) == "" {
		return Database{}, fmt.Errorf("database path is required")
	}
	if db.BackupDir == "" || db.WorkDir == "" {
		return Database{}, fmt.Errorf("backup_dir and work_dir are required (via instance defaults or explicit overrides)")
	}
	for _, p := range []string{db.Path, db.BackupDir, db.WorkDir} {
		if strings.Contains(p, "..") {
			return Database{}, fmt.Errorf("path must not contain ..")
		}
		if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
			return Database{}, fmt.Errorf("UNC paths are refused")
		}
	}
	return db, nil
}

func RegisterDatabase(cfgPath string, db Database) error {
	cfg, err := Load(cfgPath)
	if err != nil {
		return err
	}
	if _, err := cfg.Instance(db.Instance); err != nil {
		return err
	}
	normID := strings.ToLower(db.ID)
	normPath := NormalizeDBPath(db.Path)
	for _, existing := range cfg.Databases {
		if strings.ToLower(existing.ID) == normID {
			return fmt.Errorf("database id %q already exists", db.ID)
		}
		if NormalizeDBPath(existing.Path) == normPath {
			return fmt.Errorf("database path %q is already registered as %q", db.Path, existing.ID)
		}
	}
	cfg.Databases = append(cfg.Databases, db)
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return configedit.AtomicWrite(cfgPath, string(body))
}

func SuggestedDBID(dbPath string) string {
	base := strings.TrimSpace(dbPath)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	if j := strings.LastIndex(base, "."); j > 0 {
		base = base[:j]
	}
	base = strings.ToLower(base)
	var b strings.Builder
	lastUnderscore := false
	for _, r := range base {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastUnderscore = false
			continue
		}
		if !lastUnderscore {
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	id := strings.Trim(b.String(), "_")
	if id == "" || id[0] < 'a' || id[0] > 'z' {
		id = "db_" + id
	}
	return id
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
