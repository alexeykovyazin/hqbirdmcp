# fbmcp — guia rápido: instalação e uso

[English](quickstart-en.md) | [Русский](quickstart-ru.md) | **Português**

O fbmcp é um servidor MCP para Firebird 2.5–5.0. Um cliente MCP (Claude
Desktop, Claude Code, Cursor, Codex, ZCode) o usa para trabalhar com bancos
de dados como um DBA: monitoramento, backups e verificação de restore, SQL,
índices, usuários, configuração, fbagent. Toda alteração passa por uma
verificação de política e por uma confirmação humana. Todas as decisões são
gravadas no log de auditoria (`audit.jsonl`).

Detalhes: [claude-desktop.md](claude-desktop.md) (local),
[remote.md](remote.md) (remoto), [tool-reference.md](tool-reference.md) (todas as ferramentas).

Servidores HQbird 2027 já têm o fbmcp (um serviço para todas as linhas
Firebird): veja [`hqbird.md`](hqbird.md) em vez das seções 3–4.

## 1. Dois modos

| Modo | Onde o fbmcp roda | Quando usar |
|---|---|---|
| Local (stdio) | No mesmo PC do cliente | Firebird no seu PC ou na rede local |
| Remoto (HTTPS + chaves de API) | Serviço no servidor do banco | Vários administradores, servidor distante, agentes |

## 2. Obter e compilar

1. Instale o **git** e o **Go ≥ 1.25.13** (CGO não é necessário). Windows:
   ```powershell
   winget install Git.Git
   winget install GoLang.Go
   go version        # go1.25.13 ou mais novo; se "go" não for encontrado, abra uma nova janela
   ```
   Linux: git pelos pacotes da distribuição, Go em https://go.dev/dl/.
2. Baixe o código e compile:
   ```powershell
   git clone https://github.com/alexeykovyazin/hqbirdmcp.git
   cd hqbirdmcp
   $env:CGO_ENABLED = "0"
   go build -trimpath -o dist/fbmcp.exe ./cmd/fbmcp          # servidor
   go build -trimpath -o dist/fbmcpctl.exe ./cmd/fbmcpctl    # CLI do operador
   go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray  # bandeja (Windows)
   ```
3. Copie os programas para uma pasta própria (todos os exemplos usam `C:\fbmcp\`):
   ```powershell
   mkdir C:\fbmcp
   copy dist\*.exe C:\fbmcp\
   ```

No Linux, compile `fbmcp` e `fbmcpctl` da mesma forma (sem `.exe`, sem
bandeja) e copie-os para `/usr/local/bin/`.

`fbmcpctl` e `fbmcp ctl` são o mesmo programa:
`C:\fbmcp\fbmcp.exe ctl doctor` = `C:\fbmcp\fbmcpctl.exe doctor`.
`fbmcpctl help` lista os comandos; `fbmcpctl <comando> -h` mostra um.

## 3. Instalação local (Windows)

1. **Configuração** `C:\fbmcp\fbmcp.yaml` (exemplo completo: `packaging/fbmcp.yaml.example`):
   ```yaml
   state:
     dir: C:\ProgramData\fbmcp\state
   instances:
     - id: fb5
       addr: localhost:3050
       bin_dir: C:\Program Files\Firebird\Firebird_5_0   # pasta com gbak, gfix, gstat
       version: "5.0"
       service: FirebirdServerDefaultInstance
   databases:
     - id: main                       # no chat, o banco é chamado pelo id, não pelo caminho
       instance: fb5
       path: D:\Data\main.fdb
       backup_dir: D:\fbmcp\backups
       work_dir: D:\fbmcp\work
       ro_user: FBMCP_RO              # usuário somente leitura (não SYSDBA)
       ro_secret_env: FBMCP_MAIN_RO_PW
       admin_user: SYSDBA
       admin_secret_env: FBMCP_MAIN_ADMIN_PW
   ```
   Crie as pastas da configuração (o `doctor` também cria a pasta state):
   ```powershell
   mkdir C:\ProgramData\fbmcp\state, D:\fbmcp\backups, D:\fbmcp\work
   ```
2. **Usuário somente leitura.** `ro_user` é um usuário do Firebird que só
   pode ler. Crie-o uma vez, como SYSDBA, no isql
   (`isql -user SYSDBA -password <password> localhost/3050:D:\Data\main.fdb`):
   ```sql
   CREATE USER FBMCP_RO PASSWORD 'escolha-uma-senha';
   COMMIT;
   -- leitura de todas as tabelas e views deste banco;
   -- rode em cada banco, e de novo depois de criar tabelas
   SET TERM ^ ;
   EXECUTE BLOCK AS
     DECLARE r VARCHAR(63);
   BEGIN
     FOR SELECT TRIM(RDB$RELATION_NAME) FROM RDB$RELATIONS
         WHERE COALESCE(RDB$SYSTEM_FLAG, 0) = 0 INTO :r DO
       EXECUTE STATEMENT 'GRANT SELECT ON "' || r || '" TO FBMCP_RO';
   END^
   SET TERM ; ^
   COMMIT;
   ```
   Firebird 2.5: se `CREATE USER` falhar, use o gsec:
   `gsec -user SYSDBA -password <senha> -add FBMCP_RO -pw <nova senha>`.
   Um usuário que não é SYSDBA nem dono do banco vê nas tabelas MON$ só as
   próprias conexões, então o `fb_sessions` mostra menos. No Firebird 4.0 e
   5.0, permita que ele veja todas as conexões (em cada banco):
   ```sql
   CREATE ROLE FBMCP_MONITOR SET SYSTEM PRIVILEGES TO MONITOR_ANY_ATTACHMENT;
   GRANT DEFAULT FBMCP_MONITOR TO USER FBMCP_RO;
   COMMIT;
   ```
3. **Senhas** não vão no YAML. Guarde-as no cofre de credenciais do Windows
   (digite o valor, depois Ctrl+Z, Enter):
   ```powershell
   C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_RO_PW
   C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_ADMIN_PW
   ```
4. **Verificação:** `C:\fbmcp\fbmcpctl.exe doctor`. Ele encontra `C:\fbmcp\fbmcp.yaml`
   ao lado do programa (ou use `--config <arquivo>`) e conecta em cada banco
   com os dois usuários. Cada linha FAIL termina com `Fix:` e o comando a
   rodar. Repita até a última linha ser `doctor: green`.
5. **Claude Desktop:** Settings → Developer → Edit Config, adicione em `mcpServers`:
   ```json
   "fbmcp": {
     "command": "C:\\fbmcp\\fbmcp.exe",
     "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
   }
   ```
   Feche o Claude Desktop por completo (inclusive o ícone na bandeja) e abra de novo.
6. **Claude Code:**
   ```powershell
   claude mcp add --transport stdio fbmcp -- "C:\fbmcp\fbmcp.exe" -config "C:\fbmcp\fbmcp.yaml"
   ```
7. **Bandeja para aprovações** (Tier 2): `C:\fbmcp\fbmcp-tray.exe C:\fbmcp\fbmcp.yaml`.

Use caminhos absolutos em todo lugar. Não defina `listen` no modo local.
As senhas estão no cofre de credenciais do Windows (passo 3), então as
configurações dos clientes não precisam de bloco `env`.

### Cursor

Arquivo `%USERPROFILE%\.cursor\mcp.json` (todos os projetos) ou
`.cursor\mcp.json` na pasta do projeto. O formato é o mesmo do Claude Desktop:
```json
{
  "mcpServers": {
    "fbmcp": {
      "command": "C:\\fbmcp\\fbmcp.exe",
      "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
    }
  }
}
```
Depois abra Cursor Settings → MCP: o servidor `fbmcp` deve estar ativo (ponto verde).

### Codex (OpenAI Codex CLI / IDE)

Com um comando:
```powershell
codex mcp add fbmcp -- C:\fbmcp\fbmcp.exe -config C:\fbmcp\fbmcp.yaml
```
Ou à mão em `%USERPROFILE%\.codex\config.toml` (aspas simples, para não
precisar duplicar `\`):
```toml
[mcp_servers.fbmcp]
command = 'C:\fbmcp\fbmcp.exe'
args = ["-config", 'C:\fbmcp\fbmcp.yaml']
```
Verificação: `codex mcp list`.

### ZCode

Arquivo `.zcode\config.json` na pasta do projeto (chave `mcp.servers`, não `mcpServers`):
```json
{
  "mcp": {
    "servers": {
      "fbmcp": {
        "command": "C:\\fbmcp\\fbmcp.exe",
        "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
      }
    }
  }
}
```
Se o arquivo tiver senhas, adicione `.zcode/` ao `.gitignore`.

Depois de alterar a configuração, reinicie o cliente: ele carrega os servidores só na inicialização.

## 4. Instalação remota (resumo)

**Servidor Windows** (PowerShell como administrador):
1. Copie `fbmcp.exe`, `fbmcpctl.exe`, `fbmcp-tray.exe` para `C:\fbmcp\`, escreva o `fbmcp.yaml` (modelo: [remote.md §3](remote.md)).
2. Certificado (anote o pin impresso `sha256:...`):
   `C:\fbmcp\fbmcpctl.exe tls selfsign --out C:\fbmcp\tls --dns srv1 --ip 10.0.0.5 --grant "NT SERVICE\fbmcp"`
3. Uma chave por pessoa ou agente (o valor é impresso uma única vez):
   `C:\fbmcp\fbmcpctl.exe apikey new alice --max-tier 2 --secrets-dir C:\ProgramData\fbmcp\secrets`
4. Senhas e tokens do agente: `C:\fbmcp\fbmcpctl.exe secret set --dir C:\ProgramData\fbmcp\secrets NOME`
5. Instale o serviço, primeiro em modo de teste:
   `.\packaging\windows\install-remote.ps1 -Port 8443 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf`, depois sem `-WhatIf`.
6. Verificação: `C:\fbmcp\fbmcpctl.exe doctor --remote C:\fbmcp\fbmcp.yaml` → nenhuma linha FAIL.
7. Bandeja para quem aprova: `.\packaging\windows\install-tray.ps1`.

**Servidor Linux** (root):
```sh
install -m 0755 fbmcp fbmcpctl /usr/local/bin/
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24 --dry-run
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24
```
O script cria as pastas, o certificado, uma primeira configuração com a
chave `admin` (impressa uma única vez), o serviço, a regra do sudo e a
regra do ufw. Depois adicione `databases:` e `agents:` ao
`/etc/fbmcp/fbmcp.yaml`. A configuração é recarregada sozinha.

**PC do administrador:**
1. Guarde a chave recebida: `C:\fbmcp\fbmcpctl.exe secret set FBMCP_KEY_SRV1`.
2. Claude Desktop, pela ponte (bridge):
   ```json
   "fb-srv1": {
     "command": "C:\\fbmcp\\fbmcp.exe",
     "args": ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
   }
   ```
   Cursor, Codex e ZCode: a mesma entrada no arquivo de cada um (§3), só com os `args` da ponte:
   ```toml
   # Codex, config.toml
   [mcp_servers.fb-srv1]
   command = 'C:\fbmcp\fbmcp.exe'
   args = ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
   ```
   A ponte lê a chave do cofre de credenciais e confere o certificado pelo
   pin, então um certificado autoassinado não exige configurar CA no cliente.
3. Verificação: peça ao cliente para chamar `fb_ping`. A resposta mostra o nome do servidor e o seu nível de acesso.

## 5. Uso

Peça com palavras simples. Exemplos:
- "Chame `fb_ping` e `fb_db_list`."
- "Verifique a saúde do banco main" (`fb_db_health`).
- "Mostre as sessões ativas e as transações longas" (`fb_sessions`, `fb_transactions`).
- "Faça backup do main e verifique o restore" (`fb_backup_start`, `fb_restore_test`).
- "Explique o plano desta consulta e sugira índices" (`fb_analyze_query`, `fb_index_advice`).

**Níveis (Tier) e confirmações:**

| Tier | O que é | Como confirmar |
|---|---|---|
| 0 | Leitura | Não precisa |
| 1 | Alterações (DML, DDL, backup) | O cliente mostra um request id e um token; você concorda, o cliente chama `fb_confirm` |
| 2 | Ações de alto risco (reinício do serviço, restore-replace, configuração) | Só fora do chat: bandeja (Approve) ou `fbmcpctl approve <id>` no servidor |
| 3 | Críticas | Desativadas |

O Tier 2 normalmente exige uma janela de manutenção aberta (`fb_window_open`)
e um backup verificado com menos de 24 horas (o servidor informa o que
falta). Uma solicitação expira em 15 minutos. Qualquer chamada arriscada
pode ser vista antes com `mode: preview`: ela mostra o que será feito.

## 6. Operação diária

- **Aprovar uma solicitação:** `fbmcpctl approve` lista as solicitações com os comandos prontos;
  `fbmcpctl approve <id>` mostra a solicitação, pergunta y/N e aprova;
  `fbmcpctl approve --deny <id> --reason "..."` recusa. `--yes` não pergunta (scripts, `ssh`).
- **Qual configuração:** todo comando aceita `--config <arquivo>`. Sem ele: `FBMCP_CONFIG`, depois
  `fbmcp.yaml` na pasta atual, depois ao lado do programa, depois o padrão do HQbird.
  O comando mostra `using config <caminho>`.
- **Trocar uma chave:** `fbmcpctl apikey rotate alice --config <fbmcp.yaml>`. A chave antiga para de funcionar em poucos segundos.
- **Listar chaves:** `fbmcpctl apikey list` (os valores nunca aparecem).
- **Renovar o certificado:** `fbmcpctl tls selfsign --renew --out <dir> --dns ... --ip ...` (o pin não muda).
- **Verificar a auditoria:** `fbmcpctl verify`.
- **Logs:** Windows `<state_dir>\fbmcp.log`; Linux `journalctl -u fbmcp`.
- **Atualizar o fbmcp:** pare o serviço, substitua os arquivos exe, inicie, rode `doctor --remote`.

## 7. Problemas comuns

| Sintoma | O que fazer |
|---|---|
| As ferramentas não aparecem no cliente | Verifique o JSON e os caminhos absolutos; reinicie o cliente por completo |
| O servidor fecha logo após iniciar | `fbmcpctl doctor`: cada linha FAIL termina com `Fix:` e o comando |
| 401 | Chave errada: `fbmcpctl apikey list`, `doctor --remote` |
| 403 | Seu IP não está em `allow_from` ou na regra do firewall |
| Pin mismatch | A chave do certificado do servidor mudou: peça o novo pin ao administrador |
| Tier 2 "confirmation rejected" no chat | Esperado: aprove na bandeja ou com `fbmcpctl approve` |

## 8. Segurança

- Senhas e chaves só no cofre de credenciais (`fbmcpctl secret set`) ou na pasta de segredos do servidor. Nunca no YAML nem no JSON do cliente.
- Para agentes autônomos (OpenClaw e similares): uma chave separada com `--profile autonomous --allow-from <IP>`. Essa chave é somente leitura por padrão, e um operador confirma o Tier 1.
- Windows: a pasta do fbmcp e as pastas do Firebird/HQbird não devem dar permissão de escrita a todos os usuários (Authenticated Users: Modify). O script de instalação protege só a pasta do fbmcp.
