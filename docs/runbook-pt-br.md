# fbmcp — guia completo: instalação e uso

[English](runbook.md) | **Português (Brasil)**

Para quem instala e opera o fbmcp e **não** o compilou. Não é preciso
conhecer Go nem os detalhes internos do Firebird. Estado em 2026-11-09,
versão 2027.2.21 (modo local e remoto, instalação no HQbird, ponte fbagent,
grade de resultados).

Versões curtas: [English](quickstart-en.md) | [Русский](quickstart-ru.md) |
[Português (Brasil)](quickstart-pt.md). Detalhes (em inglês):
[remote.md](remote.md) (modo remoto), [agent-bridge.md](agent-bridge.md)
(fbagent), [tool-reference.md](tool-reference.md) (todas as ferramentas, tier
e argumentos), [claude-desktop.md](claude-desktop.md) (conexão do cliente no
host de desenvolvimento), ADRs em [decisions/](decisions/).

---

## 1. O que é o fbmcp

O fbmcp é um servidor MCP para Firebird 2.5–5.0 no Windows e no Linux. Um
cliente MCP (Claude Desktop, Claude Code, Cursor, Codex, ZCode e outros) o
usa para trabalhar como um DBA: monitoramento, backups e restores
verificados, SQL/DDL/DCL, índices, usuários e permissões, `firebird.conf`,
agendador, migrações e o fbagent do HQbird. Ele tem **98 ferramentas**: 51
somente leitura (Tier 0), 32 Tier 1, 14 Tier 2, 1 Tier 3 (desativada).
`fb_demo_write` só existe quando o servidor roda com `FBMCP_DEMO_TOOLS=1`.

O cliente vê apenas as ferramentas que pode usar. A lista de ferramentas
esconde: Tier 3, ferramentas acima do tier máximo da identidade,
`fb_confirm` e `fb_cancel` para uma identidade somente leitura, as
ferramentas do fbagent enquanto `agents:` estiver vazio
(`fb_agent_discover` e `fb_agent_register` continuam visíveis) e as
ferramentas de migração enquanto nenhum banco tiver `migrations_dir`. Quando
um reload da configuração muda esse conjunto, o cliente recebe uma nova
lista. Uma chamada direta a uma ferramenta escondida recebe uma recusa clara.

| Binário | Função |
|---|---|
| `fbmcp` | O servidor. Padrão: MCP via stdio (o cliente o inicia). `-serve`: sem stdio, roda como serviço (agendador, socket de attach, listener HTTPS se configurado). `fbmcp bridge`: cliente stdio para um fbmcp remoto |
| `fbmcpctl` | CLI do operador: `doctor`, `approve`, `secret`, `apikey`, `tls`, `verify`, `status`, `ping`, `setup`, `repair`, `hqbird-install`, `gate`. É o mesmo programa que `fbmcp ctl` |
| `fbmcp-tray` | Bandeja do Windows: janela Aprovar/Negar para pedidos que precisam de um operador |

Um **kernel** por pasta de estado (state). Um segundo cliente com a mesma
configuração não inicia um segundo kernel: ele se conecta (attach) ao que já
está rodando.

## 2. Modelo de segurança

Toda chamada passa por política → confirmação humana → execução auditada.

| Tier | O quê | Como é confirmado |
|---|---|---|
| 0 | Leituras (transações somente leitura garantidas pelo engine) | Não precisa |
| 1 | Mudanças de rotina: DML/DDL, backup, usuários, permissões | No chat: o cliente mostra um id de pedido e um token; a pessoa concorda; o cliente chama `fb_confirm` |
| 2 | Alto impacto: controle do serviço, restore-replace, `firebird.conf`, janela exclusiva, registro, reinício de agentes | **Somente fora do chat:** a bandeja ou `fbmcpctl approve`. O cliente nunca pode aprovar |
| 3 | Crítico (`fb_db_drop`) | Desativado por projeto |

- Tier 2 normalmente precisa de uma janela de manutenção aberta
  (`fb_window_open`, no mesmo `db`) e de um backup **verificado** com menos
  de 24 horas. A recusa diz o que falta.
- Um pedido pendente expira em **15 minutos**. Uma tentativa pelo canal
  errado não o consome; `fb_cancel` o remove.
- Toda ferramenta com confirmação aceita `mode: preview`: mostra o impacto e
  não faz nada.
- Toda decisão (permitido, pendente, aprovado, negado) vai para o
  `audit.jsonl`, com cadeia de hashes.
- Não existe página web de aprovação, e nunca deve existir: uma página que o
  modelo consegue acessar quebraria o projeto.

## 3. Escolha uma instalação

| Instalação | Onde o fbmcp roda | Quando usar | Seção |
|---|---|---|---|
| A. Local stdio | No seu PC; o cliente o inicia | Um administrador, Firebird neste PC ou na rede local | §6 |
| B. Serviço local | Serviço Windows no host do banco (`-serve`) | Agendamentos precisam rodar sem cliente aberto | §7 |
| C. Remoto | Serviço no servidor do banco, HTTPS + chaves de API | Vários administradores, servidor distante, agentes autônomos | §8 |

Na instalação A o kernel vive só enquanto um cliente roda, então **os
agendamentos só disparam nesse período**. Use B ou C para agendamentos sem
supervisão (`nightly_verify`).

**fbmcp no HQbird.** As instalações do HQbird 2027 e o fbagent instalam o
fbmcp por padrão: um serviço por servidor para todas as linhas do Firebird,
configurado por `fbmcp ctl hqbird-install`. Nesse servidor não siga §6–§8;
leia [`hqbird.md`](hqbird.md) (estrutura, modo remoto com `--allow-from`,
chaves de API).

## 4. Obter os binários

Requisitos: Go ≥ 1.25.13 (para compilar), CGO não é necessário; Firebird
2.5–5.0; fbagent ≥ 2.67 com a Local API para as ferramentas de agente
(opcional).

```powershell
git clone https://github.com/alexeykovyazin/hqbirdmcp.git
cd hqbirdmcp
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w" -o dist/fbmcp.exe ./cmd/fbmcp
go build -trimpath -ldflags "-s -w" -o dist/fbmcpctl.exe ./cmd/fbmcpctl
go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray
```

Binários para Linux (compilação cruzada no Windows, ou compile no Linux sem
`GOOS`):

```powershell
$env:GOOS = "linux"; $env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w" -o dist/fbmcp ./cmd/fbmcp
go build -trimpath -ldflags "-s -w" -o dist/fbmcpctl ./cmd/fbmcpctl
```

Checksums: `Get-FileHash dist/*` ou `sha256sum dist/*`. Outras plataformas:
[packaging/README.md](../packaging/README.md). Coloque os binários em uma
pasta só deles (os exemplos usam `C:\fbmcp\`):
`mkdir C:\fbmcp; copy dist\*.exe C:\fbmcp\`
(Linux: `install -m 0755 dist/fbmcp dist/fbmcpctl /usr/local/bin/`).

`fbmcp ctl` e `fbmcpctl` são o mesmo programa. `fbmcpctl help` lista os
comandos. Os comandos que leem a configuração aceitam `--config <arquivo>`;
sem ele, usam `FBMCP_CONFIG`, depois `fbmcp.yaml` na pasta atual, depois ao
lado do programa, depois o padrão do HQbird
(`C:\HQbirdData\fbmcp\fbmcp.yaml`, `/etc/fbmcp/fbmcp.yaml`), e mostram
`using config <caminho>`.

## 5. Configurar o `fbmcp.yaml`

Exemplo completo: [packaging/fbmcp.yaml.example](../packaging/fbmcp.yaml.example).
Configuração mínima no Windows:

```yaml
state:
  dir: C:\ProgramData\fbmcp\state
# secrets:
#   dir: C:\ProgramData\fbmcp\secrets   # instalações B e C
instances:
  - id: fb5
    addr: localhost:3050
    bin_dir: C:\Program Files\Firebird\Firebird_5_0   # pasta com gbak, gfix, gstat
    version: "5.0"
    service: FirebirdServerDefaultInstance            # Linux: firebird.opt_fb50
    service_user: SYSDBA                              # ferramentas da instância (fb_firebird_log, ...)
    service_secret_env: FBMCP_FB5_SERVICE_PW
databases:
  - id: main                 # ferramentas e chats usam o id, nunca o caminho
    instance: fb5
    path: D:\Data\main.fdb
    backup_dir: D:\fbmcp\backups
    work_dir: D:\fbmcp\work  # testes de restore; Linux: fora de /tmp
    ro_user: FBMCP_RO        # um usuário Firebird somente leitura
    ro_secret_env: FBMCP_MAIN_RO_PW
    admin_user: SYSDBA
    admin_secret_env: FBMCP_MAIN_ADMIN_PW
notify:
  webhook_url: ""            # opcional: webhook de chat para pedidos pendentes
retention:
  keep_days: 0               # 0 = nunca apagar backups
```

**Segredos.** O YAML guarda só os **nomes** das variáveis. O fbmcp procura
cada valor nesta ordem: variável de ambiente → arquivo
`<secrets.dir>/<NOME>` → keyring do sistema da conta que roda o fbmcp.

```powershell
C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_RO_PW                                     # keyring do usuário atual (instalação A)
C:\fbmcp\fbmcpctl.exe secret set --dir C:\ProgramData\fbmcp\secrets FBMCP_MAIN_RO_PW  # pasta de segredos (instalações B, C)
```

O valor é lido da entrada padrão (digite, depois Ctrl+Z, Enter; no Linux
Ctrl+D). Uma conta de serviço (LocalSystem, `NT SERVICE\...`) não enxerga o
seu keyring: use `secrets.dir` para serviços. Um arquivo de segredo alterado
vale em poucos segundos, sem reiniciar.

**Usuário somente leitura.** Crie um usuário Firebird separado para
`ro_user` (SQL:
[quickstart-pt.md §3](quickstart-pt.md#3-instalação-local-windows):
`CREATE USER FBMCP_RO`, `GRANT SELECT` em todas as tabelas e, no Firebird
4.0+, um role com `MONITOR_ANY_ATTACHMENT` para que `fb_sessions` veja todas
as conexões). SYSDBA como usuário somente leitura é só para desenvolvimento;
o `doctor` avisa sobre isso.

**Reload ao vivo.** O fbmcp aplica sozinho as mudanças no `fbmcp.yaml`
(observa o arquivo), ou com `fb_config_reload`. Não é preciso reiniciar o
cliente nem o processo. Um reload espera os jobs dos bancos alterados (30 s)
e depois aplica. Recusados: mudança de `state.dir`, caminhos de banco
duplicados, configuração remota insegura.

**Verificação:** `C:\fbmcp\fbmcpctl.exe doctor` deve terminar com
`doctor: green` (modo remoto: `doctor --remote`, sem linhas FAIL). Ele cria
a pasta state que faltar, faz login em cada banco com os dois usuários, e
cada linha FAIL termina com `Fix:` e o comando a executar.

## 6. Instalação A — local stdio

Use caminhos absolutos. Não defina `listen`. Reinicie o cliente depois de
mudar a configuração dele: os clientes carregam os servidores só ao iniciar.

| Cliente | Onde | Entrada |
|---|---|---|
| Claude Desktop | Settings → Developer → Edit Config (`%APPDATA%\Claude\claude_desktop_config.json`) | `"mcpServers": {"fbmcp": {"command": "C:\\fbmcp\\fbmcp.exe", "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]}}` |
| Claude Code | linha de comando | `claude mcp add --transport stdio fbmcp -- "C:\fbmcp\fbmcp.exe" -config "C:\fbmcp\fbmcp.yaml"` |
| Cursor | `%USERPROFILE%\.cursor\mcp.json` ou `.cursor\mcp.json` no projeto | o mesmo JSON do Claude Desktop |
| Codex | `codex mcp add fbmcp -- C:\fbmcp\fbmcp.exe -config C:\fbmcp\fbmcp.yaml` ou `%USERPROFILE%\.codex\config.toml` | `[mcp_servers.fbmcp]` `command = 'C:\fbmcp\fbmcp.exe'` `args = ["-config", 'C:\fbmcp\fbmcp.yaml']` |
| ZCode | `.zcode\config.json` no projeto | `"mcp": {"servers": {"fbmcp": { ...mesmo command e args... }}}` |

As senhas estão no keyring (§5), então não é preciso um bloco `env`. Se
mesmo assim você colocar uma senha no bloco `env` do cliente, mantenha esse
arquivo fora do git.

Depois inicie a bandeja para as aprovações Tier 2:
`C:\fbmcp\fbmcp-tray.exe C:\fbmcp\fbmcp.yaml` (a cada logon:
`.\packaging\windows\install-tray.ps1 -Bin C:\fbmcp\fbmcp-tray.exe -Config C:\fbmcp\fbmcp.yaml`).

Teste rápido no cliente: "chame `fb_ping`" (mostra o nome do servidor e a
sua identidade com o tier máximo), depois "`fb_db_list`", depois
"`fb_db_health` para main".

## 7. Instalação B — serviço Windows local

PowerShell como administrador:

```powershell
.\packaging\windows\install-service.ps1 -Bin C:\fbmcp\fbmcp.exe -Config C:\fbmcp\fbmcp.yaml
sc.exe start fbmcp
```

- O serviço roda `fbmcp -serve` como LocalSystem, reinicia em caso de falha
  e para de forma limpa quando o serviço é parado.
- Use `secrets.dir` para as senhas (§5).
- Proteja a pasta: só SYSTEM e Administrators podem escrever em `C:\fbmcp`
  (o script mostra o comando `icacls`). Uma pasta criada em `C:\` deixa
  qualquer usuário alterar os arquivos, e o serviço os executa como SYSTEM.
- O usuário da bandeja e os clientes stdio locais precisam de leitura e
  escrita na pasta state:
  `icacls C:\ProgramData\fbmcp\state /grant "<usuário>:(OI)(CI)M"`. Um
  cliente stdio com a mesma configuração então se conecta ao kernel do
  serviço.
- Prontidão: `C:\fbmcp\fbmcpctl.exe ping --config C:\fbmcp\fbmcp.yaml`.
- Log: `<state_dir>\fbmcp.log` (5 × 10 MiB; travamentos em `fbmcp-crash.log`).

Para um serviço que os administradores acessam pela rede, use a instalação
C.

## 8. Instalação C — remota (HTTPS + chaves de API)

Procedimento completo: [remote.md](remote.md). Forma curta:

**Servidor Windows** (como administrador):
1. Binários e `fbmcp.yaml` em `C:\fbmcp\` (modelo: remote.md §3: `listen`, `tls`, `identities`, `allowed_origins`, `secrets.dir`).
2. `C:\fbmcp\fbmcpctl.exe tls selfsign --out C:\fbmcp\tls --dns srv1 --ip 10.0.0.5 --grant "NT SERVICE\fbmcp"` — anote o pin `sha256:...`.
3. Uma chave por pessoa ou agente: `C:\fbmcp\fbmcpctl.exe apikey new alice --max-tier 2 --secrets-dir C:\ProgramData\fbmcp\secrets` (mostrada uma única vez). Com `--config C:\fbmcp\fbmcp.yaml` ele também adiciona a entrada em `identities:` e usa `secrets.dir`.
4. Senhas e tokens de agente: `fbmcpctl secret set --dir C:\ProgramData\fbmcp\secrets NOME`.
5. `.\packaging\windows\install-remote.ps1 -Port 8443 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf`, depois sem `-WhatIf`. Ele cria o serviço, a regra de firewall, as ACLs das pastas e os direitos sobre o serviço do Firebird.
6. `C:\fbmcp\fbmcpctl.exe doctor --remote --config C:\fbmcp\fbmcp.yaml` → nenhum FAIL; `curl.exe --cacert C:\fbmcp\tls\tls.crt https://srv1:8443/healthz` → `ok`.
7. Bandeja para quem aprova: `.\packaging\windows\install-tray.ps1`.

**Servidor Linux** (root): copie `fbmcp`, `fbmcpctl` e `packaging/linux/`, depois

```sh
install -m 0755 fbmcp fbmcpctl /usr/local/bin/
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24 --dry-run
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24
```

O script cria as pastas, o certificado, uma primeira configuração com uma
chave `admin` (mostrada uma única vez), a unit do systemd, a regra de sudo
para a unit do Firebird e a regra do ufw, e depois roda `doctor --remote`.
Adicione `databases:` e `agents:` em `/etc/fbmcp/fbmcp.yaml`. Aprovações no
Linux (sem bandeja): `sudo -u firebird fbmcpctl approve [id]`. Ele encontra
`/etc/fbmcp/fbmcp.yaml` sozinho, pergunta y/N (`--yes` não pergunta), e
`--deny <id> --reason "..."` recusa.

**PC do administrador:**
1. `C:\fbmcp\fbmcpctl.exe secret set FBMCP_KEY_SRV1` (a chave que você recebeu).
2. Qualquer cliente stdio (Claude Desktop, Cursor, Codex, ZCode): a entrada
   da §6 com estes `args`:
   `["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]`.
   O pin confere a chave do servidor, então um certificado autoassinado não
   precisa de configuração de CA.
3. O Claude Code também pode se conectar direto por HTTP (`.mcp.json`
   `"type": "http"`, cabeçalho `Authorization: Bearer ${FBMCP_KEY_SRV1}`,
   `NODE_EXTRA_CA_CERTS` para um certificado autoassinado). Não passe a
   chave com `claude mcp add --header`: ela fica gravada em texto puro.
4. Teste: `fb_ping` mostra o nome do servidor e a sua identidade.

**Agentes autônomos** (OpenClaw e similares): uma chave separada com
`apikey new openclaw-1 --profile autonomous --allow-from 10.0.0.21/32`.
Somente leitura por padrão; todo pedido Tier 1 espera um operador, e
`fb_confirm` vindo do agente é recusado. Limites: remote.md §6–7.

## 9. Ponte fbagent (opcional)

As ferramentas de agente (`fb_agent_status`, `fb_agent_job_run`, ...) usam
a Local API do fbagent do HQbird no mesmo host. Elas ficam desligadas até
`agents:` ter uma entrada.

1. No cliente: "`fb_agent_discover`". Lista os agentes locais com
   `base_url`, `instance_id` e o nome sugerido para o token. Nunca lê
   tokens.
2. O operador guarda o **`local_api.token`** do agente (não o
   `agent.auth_token`): `fbmcpctl secret set [--dir <secrets.dir>] FBMCP_AGENT_TOKEN_P<porta>`.
   Nunca digite um token no chat.
3. No cliente: "`fb_agent_register` para esse agente" → aprove na bandeja.
   O fbmcp adiciona a entrada em `agents:`, recarrega e confere a
   identidade.
4. Teste: `fb_agent_status`, `fb_agent_instances`.

`fb_agent_config_set` (mudar configurações de jobs: backup, sweep, ...)
também precisa de `config_path` na entrada e de acesso de leitura ao
`agent_config.json` em uso, além de direito de criar arquivos na pasta dele.
Esse arquivo contém o token e as credenciais do Firebird, então essa
permissão é decisão do operador ([agent-bridge.md](agent-bridge.md)). Quando
a verificação do agente falha, a ferramenta não grava nada.

## 10. Uso diário

Peça ao cliente em linguagem comum; ele escolhe as ferramentas. Exemplos:

| Tarefa | Ferramentas |
|---|---|
| Está tudo no ar? | `fb_ping`, `fb_db_list`, `fb_db_health`, `fb_service_status` |
| Ler dados | `fb_query` (somente leitura, linhas + plano + estatísticas por tabela; uma grade em clientes MCP-Apps); confira um rascunho com `fb_query_check` |
| Sessões, transações longas | `fb_sessions`, `fb_transactions`, `fb_session_kill` (Tier 1) |
| Consulta lenta | `fb_analyze_query`, `fb_index_advice` → aplique com `fb_write`; `fb_agent_perf_report` |
| Backup | `fb_backup_start` → id do job → `fb_job_status` |
| Verificar um backup | `fb_restore_test` (restaura em `work_dir`, marca o backup como verificado) |
| Backup noturno + verificação | `fb_schedule_create` com `target=nightly_verify` (confirmado uma vez; instalação B ou C) |
| Esquema / DDL | `fb_schema_list`, `fb_describe`, `fb_write` (cada comando é classificado e recebe um tier), `fb_diff_schema` |
| Migrações | `fb_migration_status`, `fb_migration_plan`, `fb_migration_apply` |
| Configuração do servidor | `fb_config_get`, `fb_config_diff`, `fb_config_set` (Tier 2) |
| Logs | `fb_firebird_log` |
| Capacidade | `fb_trends`, `fb_gstat`, `fb_index_stats` |

Os jobs são assíncronos: envie → `fb_job_status`. Os jobs de um banco rodam
um de cada vez. Um job sobrevive a um reinício do fbmcp apenas como
`interrupted`: envie de novo se ainda for necessário.

`fb_config_set` muda só a própria linha no `firebird.conf` (comentários,
indentação e fins de linha ficam iguais), guarda o arquivo antigo como
`firebird.conf.prev` e nunca reinicia o Firebird. Reinicie com
`fb_service_restart` (Tier 2): ele espera o serviço parar e voltar a rodar.

## 11. Tarefas do operador

| Tarefa | Comando |
|---|---|
| Listar / aprovar / negar pedidos | `fbmcpctl approve`; `fbmcpctl approve <id>`; `fbmcpctl approve --deny <id> --reason "..."` (ou a bandeja) |
| Trocar uma chave | `fbmcpctl apikey rotate alice --config <fbmcp.yaml>` (a chave antiga para em segundos) |
| Listar chaves | `fbmcpctl apikey list` (nomes, origens, tiers; nunca valores) |
| Remover uma pessoa | apague a entrada em `identities:`, depois `fbmcpctl secret drop --dir <pasta> NOME` |
| Renovar o certificado | `fbmcpctl tls selfsign --renew --out <pasta> --dns ... --ip ...` (pin igual, sem reiniciar) |
| Conferir a cadeia de auditoria | `fbmcpctl verify` |
| O kernel está vivo? | `fbmcpctl ping`; `fbmcpctl status` (leitura offline do estado) |
| Permitir controle do serviço | `packaging/posture/verify.ps1` ou `verify.sh`, depois `fbmcpctl setup --write-posture` |
| Atualizar | pare o serviço (ou os clientes), troque os binários, inicie, rode `doctor` |
| Remover | Windows: `sc.exe stop fbmcp; sc.exe delete fbmcp; Remove-NetFirewallRule -DisplayName fbmcp-remote`. Linux: remote.md §10. Mantenha a pasta state (auditoria), a menos que o responsável decida outra coisa |

Só no host de desenvolvimento: `.\packaging\rebuild-install.ps1 -Force`
recompila `dist\`, para e troca os binários em execução (os antigos ficam
como `*.exe~`), mescla as configurações dos clientes e roda `doctor`.

## 12. Recuperação após falha e reinício

Verificado pelo harness de caos (`internal/killharness`).

| Depois de uma falha ou kill | No reinício | Ação do operador |
|---|---|---|
| Job rodando ou na fila | Marcado `interrupted`; nunca é retomado | Envie de novo se necessário |
| Pedido pendente, não confirmado | Restaurado do `state.json` (o TTL continua valendo) | Confirme ou deixe expirar |
| `fb_restore_replace` em andamento | Termina: restore, validação, banco online; `<db>.pre-restore` é mantido | Confira `fb_db_health`; apague `.pre-restore` depois |
| `fb_shutdown_window` em andamento | Termina o passo `gfix -online` | Confira se está online |
| `instance.lock` antigo | Assumido automaticamente | Nenhuma |
| `state.json` corrompido | **O kernel se recusa a iniciar**; cópia guardada como `state.json.corrupt-<ts>` | Inspecione; conserte ou remova o `state.json` de propósito (`fbmcpctl repair` reconstrói as permissões dos agendamentos a partir da auditoria) |
| `audit.jsonl` corrompido ou cortado | **O kernel se recusa a iniciar** | §13.2: trate como sinal de adulteração |
| Relógio voltou no tempo | O agendador pula; sem execução dupla | Nenhuma |
| Processo do serviço Windows morto | O SCM o reinicia em cerca de 5 s | Nenhuma |

O kernel **falha fechado** em tudo o que não consegue provar intacto. Um
loop de reinícios é um achado: leia o log.

## 13. Roteiros para incidentes

### 13.1 Algo está errado com um banco
1. `fbmcpctl doctor --config <arquivo>`: configuração, segredos, logins no
   Firebird, pastas, postura, agendamentos. Cada linha FAIL tem um `Fix:`.
2. `fb_db_list` → `fb_db_health <db>`; `fb_service_status` para a instância.
3. `fb_sessions`, `fb_transactions` para problemas de conexões ou de
   distância da OAT; `fb_firebird_log` para erros do engine.

### 13.2 Suspeita de adulteração ou corrupção: auditoria primeiro
1. `fbmcpctl verify --config <arquivo>` → `audit chain OK: N entries` ou `BROKEN` com o motivo.
2. Se falhar: guarde a pasta state inteira (ela é a evidência) e depois
   investigue. Não apague o `audit.jsonl` para fazer o kernel iniciar.

### 13.3 Treino de restore (a cada trimestre)
1. `fb_backup_start` → `fb_job_status` = succeeded.
2. `fb_restore_test` → o backup fica verificado.
3. `fb_window_open` para o banco.
4. `fb_restore_replace` → aprove fora do chat. Indisponível até ficar online.
5. Confira: `fb_db_health`, depois uma leitura real (`fb_schema_list`, `fb_info`).
6. O arquivo antigo fica como `<db>.pre-restore` até você apagá-lo.

### 13.4 O cliente não conecta
1. `fbmcpctl ping --config <arquivo>`, `fbmcpctl status --config <arquivo>`.
2. Leia o log MCP do cliente (stdio) ou `<state_dir>\fbmcp.log` / `journalctl -u fbmcp` (serviço).
3. "another fbmcp instance is active": um kernel por pasta state. Clientes
   via pipe se conectam sozinhos; um segundo kernel falha rápido por
   projeto.

Nos comandos acima, `--config` pode ser omitido quando a busca da §4
encontra o arquivo.

## 14. Problemas comuns

| Sintoma | Significado / solução |
|---|---|
| As ferramentas não aparecem | Sintaxe do JSON, caminhos absolutos, reinício completo do cliente; o log MCP do cliente |
| Falta uma ferramenta na lista | Escondida de propósito (§1): acima do seu tier máximo, sem entrada em `agents:` ou sem `migrations_dir` |
| O servidor sai na hora | `fbmcpctl doctor`; um segredo faltando; um serviço não lê o seu keyring (use `secrets.dir`) |
| Tier 2 "confirmation rejected: channel policy" | Esperado no chat. Aprove na bandeja ou com `fbmcpctl approve` |
| Tier 2 "no open maintenance window" | `fb_window_open` para o **mesmo** `db` que você passa para a ferramenta |
| `fb_restore_replace` negado: preconditions | Precisa de um backup verificado < 24 h: `fb_backup_start` + `fb_restore_test`, e de uma janela |
| Controle do serviço negado | Falta `posture.verified` (`fbmcpctl setup --write-posture`) ou a conta não tem direitos sobre o serviço do Firebird |
| `fb_config_set`: não consegue gravar | A conta não pode alterar esse arquivo (unit Linux com `ProtectSystem=strict`; ACL no Windows). Edite à mão, ou veja remote.md §8 |
| A bandeja não mostra nada | Ela mostra só pedidos que precisam de um operador; confira os direitos do usuário da bandeja na pasta state e o `tray.log` lá |
| 401 (remoto) | Chave errada, ou o servidor não a carregou: `apikey list`, `doctor --remote` |
| 403 (remoto) | IP de origem fora de `allow_from` / firewall, ou cabeçalho Origin fora de `allowed_origins` |
| 429 (remoto) | Limites; espere o `Retry-After` |
| Pin não confere | A chave do servidor mudou; peça o novo pin ao administrador do servidor |
| `agent-unreachable` | Serviço do agente parado, `local_api.enabled: false`, ou `base_url` errado (padrão `127.0.0.1:(10000 + porta do firebird)`) |
| `agent-auth` | O token guardado não é o `local_api.token` do agente; grave de novo |
| `agent-timeout` em reinícios | `agents[].timeout_seconds` (130) deve ser maior que o `restart_timeout_seconds` do agente (120) |
| `Error loading plugin MySQLEngine …` no attach | No HQbird isso normalmente significa que o **arquivo do banco não existe**; confira o caminho |
| `DROP INDEX` puro em `fb_write` pede janela | Lá ele é Tier 2; use `fb_index_drop` (Tier 1; recusa índices de constraints) |
| Índice novo não é usado | Atualize as estatísticas (`fb_index_rebuild` action=statistics); `ORDER BY … DESC` precisa de um índice `DESCENDING` |
| Ferramentas falham só para um banco | Ele está offline (`fb_db_health`); as ferramentas degradam por banco |

## 15. Referência da pasta state

| Caminho | Significado |
|---|---|
| `state.json` (+ `.tmp`, `.corrupt-*`) | Pedidos pendentes, jobs, catálogo de backups, workflows, agendamentos. Corrompido ⇒ recusa iniciar |
| `audit.jsonl` (+ `.head`) | Log de auditoria com cadeia de hashes; `.head` detecta truncamento |
| `events.jsonl` | Eventos de jobs (`job.succeeded`, `job.failed` com a mensagem) |
| `approvals\`, `denials\` | Decisões fora do chat (bandeja / `fbmcpctl approve`), usadas em ~2 s |
| `instance.lock` | Trava de um kernel só |
| `attach.addr`, `attach.token` | Ponto de attach para clientes stdio extras |
| `fbmcp.log`, `fbmcp-crash.log` | Log do serviço (rotacionado) e saída de travamentos |
| `query-log.jsonl` | Toda chamada de `fb_query` |
| `trends\` | Histórico do amostrador para `fb_trends` |
| `config-journal\` | Histórico do `fb_config_set` (valor antigo e novo) |
| `posture.verified` | Marcador que permite o controle do serviço |
| `tray.log` | Erros da bandeja |

## 16. Status do roteiro de teste

Pendente (M1): percorrer este guia em um host Windows novo, usando só ele.
Os achados voltam para este arquivo (versão em inglês, [runbook.md](runbook.md)).
Resultados de laboratório do modo remoto (host Linux, VM nova, serviço
Windows): [findings/phase12-lab-2026-11-03.md](findings/phase12-lab-2026-11-03.md).
