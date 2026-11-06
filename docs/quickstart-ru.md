# fbmcp — краткое руководство: установка и использование

[English](quickstart-en.md) | **Русский** | [Português](quickstart-pt.md)

fbmcp — это MCP-сервер для Firebird 2.5–5.0. Через него MCP-клиент (Claude
Desktop, Claude Code, Cursor, Codex, ZCode) работает с базами как DBA: мониторинг,
бэкапы и проверка восстановления, SQL, индексы, пользователи, конфиг,
fbagent. Каждое изменение проходит проверку политики и подтверждение
человеком. Все решения пишутся в журнал аудита (`audit.jsonl`).

Подробно: [claude-desktop.md](claude-desktop.md) (локально),
[remote.md](remote.md) (удалённо), [tool-reference.md](tool-reference.md) (все инструменты).

На серверах HQbird 2027 fbmcp уже установлен (одна служба на все линии
Firebird): вместо разделов 3–4 смотрите [`hqbird.md`](hqbird.md).

## 1. Два режима

| Режим | Где работает fbmcp | Когда выбрать |
|---|---|---|
| Локальный (stdio) | На том же ПК, что и Claude | Firebird на вашем ПК или в локальной сети |
| Удалённый (HTTPS + API-ключи) | Служба на сервере БД | Несколько админов, сервер в другом месте, агенты |

## 2. Получить и собрать

1. Установите **git** и **Go ≥ 1.25.13** (CGO не нужен). Windows:
   ```powershell
   winget install Git.Git
   winget install GoLang.Go
   go version        # go1.25.13 или новее; если команды "go" нет, откройте новое окно
   ```
   Linux: git из пакетов дистрибутива, Go с https://go.dev/dl/.
2. Скачайте исходники и соберите:
   ```powershell
   git clone https://github.com/alexeykovyazin/hqbirdmcp.git
   cd hqbirdmcp
   $env:CGO_ENABLED = "0"
   go build -trimpath -o dist/fbmcp.exe ./cmd/fbmcp          # сервер
   go build -trimpath -o dist/fbmcpctl.exe ./cmd/fbmcpctl    # CLI оператора
   go build -trimpath -ldflags "-s -w -H=windowsgui" -o dist/fbmcp-tray.exe ./cmd/fbmcp-tray  # трей (Windows)
   ```
3. Скопируйте программы в отдельную папку (во всех примерах `C:\fbmcp\`):
   ```powershell
   mkdir C:\fbmcp
   copy dist\*.exe C:\fbmcp\
   ```

На Linux соберите `fbmcp` и `fbmcpctl` так же (без `.exe`, трей не нужен)
и скопируйте их в `/usr/local/bin/`.

`fbmcpctl` и `fbmcp ctl` — одна и та же программа:
`C:\fbmcp\fbmcp.exe ctl doctor` = `C:\fbmcp\fbmcpctl.exe doctor`.
`fbmcpctl help` показывает список команд, `fbmcpctl <команда> -h` — одну команду.

## 3. Локальная установка (Windows)

1. **Конфиг** `C:\fbmcp\fbmcp.yaml` (полный пример: `packaging/fbmcp.yaml.example`):
   ```yaml
   state:
     dir: C:\ProgramData\fbmcp\state
   instances:
     - id: fb5
       addr: localhost:3050
       bin_dir: C:\Program Files\Firebird\Firebird_5_0   # папка с gbak, gfix, gstat
       version: "5.0"
       service: FirebirdServerDefaultInstance
   databases:
     - id: main                       # в чате базу называют по id, не по пути
       instance: fb5
       path: D:\Data\main.fdb
       backup_dir: D:\fbmcp\backups
       work_dir: D:\fbmcp\work
       ro_user: FBMCP_RO              # пользователь только для чтения (не SYSDBA)
       ro_secret_env: FBMCP_MAIN_RO_PW
       admin_user: SYSDBA
       admin_secret_env: FBMCP_MAIN_ADMIN_PW
   ```
   Создайте папки из конфига (папку state `doctor` создаст и сам):
   ```powershell
   mkdir C:\ProgramData\fbmcp\state, D:\fbmcp\backups, D:\fbmcp\work
   ```
2. **Пользователь только для чтения.** `ro_user` — пользователь Firebird,
   который может только читать. Создайте его один раз под SYSDBA в isql
   (`isql -user SYSDBA -password <password> localhost/3050:D:\Data\main.fdb`):
   ```sql
   CREATE USER FBMCP_RO PASSWORD 'выберите-пароль';
   COMMIT;
   -- чтение всех таблиц и представлений этой базы;
   -- выполните в каждой базе и ещё раз после добавления таблиц
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
   Firebird 2.5: если `CREATE USER` не работает, используйте gsec:
   `gsec -user SYSDBA -password <пароль> -add FBMCP_RO -pw <новый пароль>`.
   Пользователь, который не SYSDBA и не владелец базы, видит в таблицах MON$
   только свои подключения, поэтому `fb_sessions` покажет меньше. На
   Firebird 4.0 и 5.0 разрешите ему видеть все подключения (в каждой базе):
   ```sql
   CREATE ROLE FBMCP_MONITOR SET SYSTEM PRIVILEGES TO MONITOR_ANY_ATTACHMENT;
   GRANT DEFAULT FBMCP_MONITOR TO USER FBMCP_RO;
   COMMIT;
   ```
3. **Пароли** — не в YAML. Сохраните их в хранилище Windows (значение
   вводится с клавиатуры, затем Ctrl+Z, Enter):
   ```powershell
   C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_RO_PW
   C:\fbmcp\fbmcpctl.exe secret set FBMCP_MAIN_ADMIN_PW
   ```
4. **Проверка:** `C:\fbmcp\fbmcpctl.exe doctor`. Он находит `C:\fbmcp\fbmcp.yaml`
   рядом с программой (или укажите `--config <файл>`) и подключается к каждой
   базе под обоими пользователями. Каждая строка FAIL заканчивается `Fix:` и
   командой. Повторяйте, пока последняя строка не станет `doctor: green`.
5. **Claude Desktop:** Settings → Developer → Edit Config, добавьте в `mcpServers`:
   ```json
   "fbmcp": {
     "command": "C:\\fbmcp\\fbmcp.exe",
     "args": ["-config", "C:\\fbmcp\\fbmcp.yaml"]
   }
   ```
   Полностью закройте Claude Desktop (и значок в трее) и запустите снова.
6. **Claude Code:**
   ```powershell
   claude mcp add --transport stdio fbmcp -- "C:\fbmcp\fbmcp.exe" -config "C:\fbmcp\fbmcp.yaml"
   ```
7. **Трей для подтверждений** (Tier 2): `C:\fbmcp\fbmcp-tray.exe C:\fbmcp\fbmcp.yaml`.

Пути везде абсолютные. Параметр `listen` в локальном режиме не задавайте.
Пароли лежат в хранилище Windows (шаг 3), поэтому блок `env` в настройках
клиентов не нужен.

### Cursor

Файл `%USERPROFILE%\.cursor\mcp.json` (для всех проектов) или
`.cursor\mcp.json` в папке проекта. Формат тот же, что у Claude Desktop:
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
Затем Cursor Settings → MCP: сервер `fbmcp` должен быть включён (зелёная точка).

### Codex (OpenAI Codex CLI / IDE)

Командой:
```powershell
codex mcp add fbmcp -- C:\fbmcp\fbmcp.exe -config C:\fbmcp\fbmcp.yaml
```
Или вручную в `%USERPROFILE%\.codex\config.toml` (одинарные кавычки — чтобы
не удваивать `\`):
```toml
[mcp_servers.fbmcp]
command = 'C:\fbmcp\fbmcp.exe'
args = ["-config", 'C:\fbmcp\fbmcp.yaml']
```
Проверка: `codex mcp list`.

### ZCode

Файл `.zcode\config.json` в папке проекта (ключ `mcp.servers`, а не `mcpServers`):
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
Если в файле есть пароли, добавьте `.zcode/` в `.gitignore`.

После правки конфига перезапустите клиент: серверы загружаются только при старте.

## 4. Удалённая установка (кратко)

**Сервер Windows** (PowerShell от администратора):
1. Скопируйте `fbmcp.exe`, `fbmcpctl.exe`, `fbmcp-tray.exe` в `C:\fbmcp\`, напишите `fbmcp.yaml` (шаблон: [remote.md §3](remote.md)).
2. Сертификат (запишите напечатанный pin `sha256:...`):
   `C:\fbmcp\fbmcpctl.exe tls selfsign --out C:\fbmcp\tls --dns srv1 --ip 10.0.0.5 --grant "NT SERVICE\fbmcp"`
3. Ключ для каждого человека или агента (значение печатается один раз):
   `C:\fbmcp\fbmcpctl.exe apikey new alice --max-tier 2 --secrets-dir C:\ProgramData\fbmcp\secrets`
4. Пароли и токены агента: `C:\fbmcp\fbmcpctl.exe secret set --dir C:\ProgramData\fbmcp\secrets ИМЯ`
5. Установка службы, сначала пробный прогон:
   `.\packaging\windows\install-remote.ps1 -Port 8443 -AllowFrom 10.0.0.0/24 -FirebirdService FirebirdServerDefaultInstance -WhatIf`, потом без `-WhatIf`.
6. Проверка: `C:\fbmcp\fbmcpctl.exe doctor --remote C:\fbmcp\fbmcp.yaml` → нет строк FAIL.
7. Трей для того, кто подтверждает: `.\packaging\windows\install-tray.ps1`.

**Сервер Linux** (root):
```sh
install -m 0755 fbmcp fbmcpctl /usr/local/bin/
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24 --dry-run
sh packaging/linux/install-remote.sh --user firebird --port 8443 --allow-from 10.0.0.0/24
```
Скрипт создаёт папки, сертификат, первый конфиг с ключом `admin` (печатается
один раз), службу, правило sudo и ufw. Затем допишите в `/etc/fbmcp/fbmcp.yaml`
разделы `databases:` и `agents:`. Конфиг перечитывается сам.

**ПК администратора:**
1. Сохраните полученный ключ: `C:\fbmcp\fbmcpctl.exe secret set FBMCP_KEY_SRV1`.
2. Claude Desktop — через мост:
   ```json
   "fb-srv1": {
     "command": "C:\\fbmcp\\fbmcp.exe",
     "args": ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
   }
   ```
   Cursor, Codex и ZCode — та же запись в их файле (§3), только с `args` моста:
   ```toml
   # Codex, config.toml
   [mcp_servers.fb-srv1]
   command = 'C:\fbmcp\fbmcp.exe'
   args = ["bridge", "--url", "https://srv1:8443/mcp", "--key-env", "FBMCP_KEY_SRV1", "--pin", "sha256:<hex>"]
   ```
   Мост берёт ключ из хранилища и проверяет сертификат по pin, поэтому
   самоподписанный сертификат не требует настройки CA в клиенте.
3. Проверка: попросите клиента вызвать `fb_ping` — ответ покажет имя сервера и ваш уровень доступа.

## 5. Использование

Просите Claude обычными словами. Примеры:
- «Вызови `fb_ping` и `fb_db_list`.»
- «Проверь здоровье базы main» (`fb_db_health`).
- «Покажи активные сессии и долгие транзакции» (`fb_sessions`, `fb_transactions`).
- «Сделай бэкап main и проверь восстановление» (`fb_backup_start`, `fb_restore_test`).
- «Разбери план этого запроса и предложи индексы» (`fb_analyze_query`, `fb_index_advice`).

**Уровни (Tier) и подтверждения:**

| Tier | Что это | Как подтвердить |
|---|---|---|
| 0 | Чтение | Не нужно |
| 1 | Изменения (DML, DDL, бэкап) | Claude показывает request id и токен; вы соглашаетесь, Claude вызывает `fb_confirm` |
| 2 | Опасные действия (рестарт службы, restore-replace, конфиг) | Только вне чата: трей (Approve) или `fbmcpctl approve <id>` на сервере |
| 3 | Критичные | Отключены |

Для Tier 2 обычно нужны: открытое окно обслуживания (`fb_window_open`) и
проверенный бэкап не старше 24 часов (сервер скажет, чего не хватает).
Запрос живёт 15 минут. Любой опасный вызов можно сначала посмотреть с
`mode: preview` — он покажет, что будет сделано.

## 6. Обслуживание

- **Подтвердить запрос:** `fbmcpctl approve` показывает запросы и готовые команды;
  `fbmcpctl approve <id>` показывает запрос, спрашивает y/N и подтверждает;
  `fbmcpctl approve --deny <id> --reason "..."` отклоняет. `--yes` — без вопроса (скрипты, `ssh`).
- **Какой конфиг:** у каждой команды есть `--config <файл>`. Без него: `FBMCP_CONFIG`, затем
  `fbmcp.yaml` в текущей папке, затем рядом с программой, затем путь HQbird по умолчанию.
  Команда пишет `using config <путь>`.
- **Сменить ключ:** `fbmcpctl apikey rotate alice --config <fbmcp.yaml>`. Старый ключ перестаёт работать через несколько секунд.
- **Список ключей:** `fbmcpctl apikey list` (значения не показываются).
- **Обновить сертификат:** `fbmcpctl tls selfsign --renew --out <dir> --dns ... --ip ...` (pin не меняется).
- **Проверить аудит:** `fbmcpctl verify`.
- **Логи:** Windows `<state_dir>\fbmcp.log`; Linux `journalctl -u fbmcp`.
- **Обновить fbmcp:** остановить службу, заменить exe, запустить, `doctor --remote`.

## 7. Если не работает

| Симптом | Что делать |
|---|---|
| Инструменты не появились в Claude | Проверить JSON, абсолютные пути, полностью перезапустить Claude |
| Сервер сразу завершается | `fbmcpctl doctor`: каждая строка FAIL заканчивается `Fix:` и командой |
| 401 | Неверный ключ: `fbmcpctl apikey list`, `doctor --remote` |
| 403 | Ваш IP не в `allow_from` или правиле файрвола |
| Pin mismatch | Ключ сертификата сервера сменился — взять новый pin у админа |
| Tier 2 «confirmation rejected» в чате | Так и должно быть: подтверждать в трее или `fbmcpctl approve` |

## 8. Безопасность

- Пароли и ключи — только в хранилище (`fbmcpctl secret set`) или в папке секретов сервера. Не в YAML и не в JSON клиента.
- Для автономных агентов (OpenClaw и т. п.): отдельный ключ с `--profile autonomous --allow-from <IP>`. Такой ключ по умолчанию только читает, а Tier 1 подтверждает оператор.
- Windows: папка fbmcp и папки Firebird/HQbird не должны давать запись всем пользователям (Authenticated Users: Modify). Скрипт установки защищает только папку fbmcp.
