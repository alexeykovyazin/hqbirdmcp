package facts

import (
	"github.com/aleks/fbmcp/internal/config"
	fb "github.com/nakagami/firebirdsql"
)

type serviceClient interface {
	Version() (string, error)
	ConnectedDBInfo() (*fb.SrvDbInfo, error)
	Close() error
}

type svcMgr struct {
	s *fb.ServiceManager
}

var openSvc = func(addr, user, pass string) (serviceClient, error) {
	return newSvcMgr(addr, user, pass)
}

func newSvcMgr(addr, user, pass string) (*svcMgr, error) {
	s, err := fb.NewServiceManager(addr, user, pass, fb.NewServiceManagerOptions())
	if err != nil {
		return nil, err
	}
	return &svcMgr{s}, nil
}

func (m *svcMgr) Version() (string, error) { return m.s.GetServerVersionString() }
func (m *svcMgr) ConnectedDBInfo() (*fb.SrvDbInfo, error) {
	return m.s.GetSvrDbInfo()
}
func (m *svcMgr) Close() error { return m.s.Close() }

type ConnectedDB struct {
	Path        string
	DBID        string
	MatchStatus string
}

type ConnectedDBs struct {
	Instance         string
	AttachmentsCount int
	DatabaseCount    int
	Databases        []ConnectedDB
}

func ConnectedDatabases(cfg *config.Config, instID string) (*ConnectedDBs, error) {
	inst, err := cfg.Instance(instID)
	if err != nil {
		return nil, err
	}
	if err := inst.ValidateDiscoveryDefaults(); err != nil {
		return nil, err
	}
	pass, err := config.SecretFromEnv(inst.ServiceSecretEnv)
	if err != nil {
		return nil, err
	}
	svc, err := openSvc(inst.Addr, inst.ServiceUser, pass)
	if err != nil {
		return nil, err
	}
	defer svc.Close()
	raw, err := svc.ConnectedDBInfo()
	if err != nil {
		return nil, err
	}
	return mapConnectedDatabases(cfg, inst.ID, raw), nil
}

func mapConnectedDatabases(cfg *config.Config, instID string, raw *fb.SrvDbInfo) *ConnectedDBs {
	out := &ConnectedDBs{Instance: instID}
	if raw == nil {
		return out
	}
	out.AttachmentsCount = raw.AttachmentsCount
	out.DatabaseCount = raw.DatabaseCount
	for _, p := range raw.Databases {
		entry := ConnectedDB{Path: p}
		var hits []string
		np := config.NormalizeDBPath(p)
		for _, db := range cfg.Databases {
			if db.Instance != instID {
				continue
			}
			if config.NormalizeDBPath(db.Path) == np {
				hits = append(hits, db.ID)
			}
		}
		switch len(hits) {
		case 0:
			entry.MatchStatus = "unmanaged"
		case 1:
			entry.MatchStatus = "managed"
			entry.DBID = hits[0]
		default:
			entry.MatchStatus = "ambiguous"
		}
		out.Databases = append(out.Databases, entry)
	}
	return out
}
