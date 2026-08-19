# fbmcp packaging (P5.2)

Cross-compile (CGO_ENABLED=0, ADR-026 recipe — not bitwise-identical):

```
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=dev" -o dist/fbmcp-windows-amd64.exe ./cmd/fbmcp
GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=dev" -o dist/fbmcp-linux-amd64 ./cmd/fbmcp
GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=dev" -o dist/fbmcp-linux-arm64 ./cmd/fbmcp
go build -trimpath -o dist/fbmcpctl.exe ./cmd/fbmcpctl
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray
```

Checksums: `Get-FileHash dist/*` / `sha256sum dist/*`. SBOM: `go version -m dist/fbmcp-*` (feeds P6.1).

Windows service: [windows/install-service.ps1](windows/install-service.ps1) — recovery = restart on failure.

Windows tray Approve/Deny (Tier ≥ 2 out-of-band confirmation): [windows/install-tray.ps1](windows/install-tray.ps1). The service normally runs as LocalSystem and owns `state.dir`; the interactive user running fbmcp-tray needs read/write access to it (list+write on `approvals/`/`denials/`, read on `state.json`) — grant it with `icacls <state.dir> /grant "<user>:(OI)(CI)M"` if the operator account isn't already an owner/admin on that path.

systemd: [linux/fbmcp.service](linux/fbmcp.service)

Docker: [docker/Dockerfile](docker/Dockerfile) — Linux matrix is **best-effort / deferred** if containers are not running on this host.

Posture verify (no mutation): [posture/verify.ps1](posture/verify.ps1) / [posture/verify.sh](posture/verify.sh). After green: `fbmcpctl setup --write-posture`.

Claude Desktop / Claude Code (stdio): [../docs/claude-desktop.md](../docs/claude-desktop.md) · example JSON: [claude_desktop_config.example.json](claude_desktop_config.example.json).
