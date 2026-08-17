package facts

import (
	fb "github.com/nakagami/firebirdsql"
)

type svcMgr struct {
	s *fb.ServiceManager
}

func newSvcMgr(addr, user, pass string) (*svcMgr, error) {
	s, err := fb.NewServiceManager(addr, user, pass, fb.NewServiceManagerOptions())
	if err != nil {
		return nil, err
	}
	return &svcMgr{s}, nil
}

func (m *svcMgr) Version() (string, error) { return m.s.GetServerVersionString() }
func (m *svcMgr) Close() error             { return m.s.Close() }
