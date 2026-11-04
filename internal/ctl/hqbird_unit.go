package ctl

import (
	"bytes"
	"strings"
	"text/template"
)

// unitTemplate follows packaging/linux/fbmcp.service (a test compares them).
var unitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=fbmcp - Firebird DBA MCP server (HQbird)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Written by "fbmcp ctl hqbird-install"; run it again instead of editing.
User={{.User}}
Group={{.User}}
ExecStart={{.Exe}} -config {{.Config}} -serve
EnvironmentFile=-{{.ConfigDir}}/fbmcp.env
Restart=on-failure
RestartSec=5
TimeoutStopSec=30
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths={{range .RW}}-{{.}} {{end}}

[Install]
WantedBy=multi-user.target
`))

type unitData struct {
	User, Exe, Config, ConfigDir string
	RW                           []string
}

func renderUnit(d unitData) string {
	var b bytes.Buffer
	_ = unitTemplate.Execute(&b, d)
	return strings.Replace(b.String(), " \n", "\n", 1)
}
