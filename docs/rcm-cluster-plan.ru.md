# fbmcp и HQbird RCM: кластер через команды fbmcp

Дата: 2026-11-12. Ветка fbmcp: `feat/rcm-cluster`.

## 1. Что есть сейчас (fbmcp 2027.2.21)

fbmcp **не умеет** ставить кластер. Связь с RCM есть только в трёх местах:

- `fb_agent_product_update_status` / `fb_agent_product_update_apply`:
  обновление продуктов `hqclusternode` и `hqbirdrcm` через fbagent.
- `fb_agent_restart` / `stop` / `start`: отказ, если хостом управляет
  `hqclusternode` (флаг `cluster_override`).
- `fb_agent_discover`: замечание «hqclusternode detected».

fbmcp не знает адрес RCM, не имеет учётной записи RCM и не вызывает API
RCM. Кластер сегодня ставят в UI RCM или CLI `hqbirdrcm` на мастере
(goafts `docs/hqbird2027-linux-cluster-runbook.md`).

## 2. Как кластер ставится сейчас (кит 2027.2.2x)

1. `.run` на каждом сервере: Firebird, fbagent, HQ Monitor, `fbagent
   --cluster-init` (свой RCM на :7444, CA кластера, окно pairing :7450 на
   24 ч), потом fbmcp (`fbmcp ctl hqbird-install`, от root).
2. Вход в RCM мастера: `rcmadmin`, первый пароль в
   `<agent>/clusterca/<cluster>/rcm-initial-password`.
3. Link hosts: `POST /v1/admin/pair/probe` → `POST /v1/admin/pair/link` →
   опрос `GET /v1/admin/pair/link/{id}`.
4. Папка реплики: `POST /v1/nodes/{RN}/databases/root/prepare` +
   `PUT /v1/nodes/{RN}/config {"databases":{"root":…}}`.
5. Enroll на мастере: `POST /v1/nodes/{MN}/databases/enroll`.
6. Рестарт Firebird мастера: `POST /v1/nodes/{MN}/firebird/restart`.
7. Публикация: `POST /v1/nodes/{MN}/publication/sync` (+ опрос команды).
8. Инициализация реплики: `POST /v1/nodes/{MN}/reinit` → `GET /v1/reinit/{id}`.
9. Проверка: `POST /v1/verify` → `GET /v1/verify/{id}`.

API оператора RCM: HTTPS на 127.0.0.1:7444 (сертификат от CA кластера),
HTTP Digest (Basic запрещён). Digest-запрос не блокируется флагом
`must_change` первого пароля.

## 3. Цель

С MCP-клиента, подключённого к fbmcp мастера: подключить fbmcp к RCM,
связать хосты, поставить базы под репликацию и запустить реплику — только
инструментами fbmcp. SSH нужен только для `fbmcp ctl approve` (Tier 2, как
сейчас у всех Tier 2 инструментов).

## 4. Доработки

### Этап A. fbmcp ↔ RCM (только fbmcp, работает с китом 2027.2.28)

A1. **Конфиг `rcm:`** в `fbmcp.yaml`: `url`, `ca_file`, `user`,
`password_env` (имя секрета, по умолчанию `FBMCP_RCM_PASSWORD`). Только
loopback, как у `agents`, если нет `allow_non_loopback`.

A2. **Клиент `internal/rcmapi`**: HTTPS с CA кластера, HTTP Digest
(MD5, qop=auth), заголовок `X-Admin-User` = личность fbmcp (аудит RCM
пишет, кто вызвал), разбор ошибок RCM `{"error":{"code","message"}}`.

A3. **`fbmcp ctl hqbird-install`** находит RCM хоста
(`/opt/hqbirdrcm/rcm.json`): пишет секцию `rcm:` (управляет ею, как
`agents`), копирует первый пароль из `clusterca/*/rcm-initial-password` в
секрет `FBMCP_RCM_PASSWORD`, если секрета ещё нет. Оператору ничего
вводить не нужно.

A4. **Инструменты** (новая группа `fb_cluster_*`):

| Инструмент | Tier | Что делает |
|---|---|---|
| `fb_cluster_status` | 0 | обзор (здоровье RCM, кластер, узлы, базы) или `view` = nodes, databases, commands, command, alerts, releases, node, config, reinit, link |
| `fb_cluster_probe` | 0 | проверка хостов перед связью: отпечаток, Member ID, состояние |
| `fb_cluster_verify` | 0 | сравнение `COUNT(*)` мастер/реплика, ждёт результат |
| `fb_cluster_rcm_password` | 1 | новый случайный пароль `rcmadmin` (снимает `must_change`), сохраняет в секрет fbmcp; пароль не выводится |
| `fb_cluster_link` | 1 | связать хосты; запрос показывает отпечатки (сравнить со строкой `Pairing:` установщика), при выполнении отпечатки должны совпасть с показанными |
| `fb_cluster_replica_folder` | 1 | создать папку реплики на узле и записать `databases.root` |
| `fb_cluster_enroll` | 1 | поставить базы папки под управление; запрос показывает dry-run |
| `fb_cluster_publication` | 1 | включить/синхронизировать публикацию (вся нода или одна база) |
| `fb_cluster_firebird_restart` | 2 | рестарт Firebird узла через узел (окно на `db`, OOB) |
| `fb_cluster_reinit` | 2 | инициализация/реинициализация реплики, ждёт конца, шаги и ошибка RCM в ответе (окно на `db`, OOB) |

Инструменты видны, только если в конфиге есть `rcm:`.

A5. **Тесты**: unit-тесты клиента (Digest, TLS, ошибки) на httptest;
тесты инструментов на заглушке RCM; drift-тесты (policy, tool-reference,
README).

A6. **Документация**: `docs/tool-reference.md` (раздел HQbird cluster),
`docs/hqbird.md`, playbook `prompts/cluster-setup.md`.

### Этап B. Окно pairing через fbagent (fbagent + fbmcp)

Окно pairing открыто 24 ч после установки. Потом его открывает только
`sudo -u firebird fbagent --cluster-pairing on` на хосте.

B1. fbagent Local API (capability `cluster_pairing`):
`GET /v1/cluster/pairing` (state open|closed, reason, expires, отпечаток,
rcm_allow, standing) и `POST /v1/cluster/pairing`
`{"on":true,"hours":N,"rcm_allow":[...]}` / `{"on":false}` — то же, что
`fbagent --cluster-pairing on|off`. Окно 1–24 ч (0 = 24). Токен Local API,
аудит с `requested_by` и `reason`. Сделано: fbagent `dcfa2c03`, `540cff43`.

B2. fbmcp: один инструмент `fb_agent_cluster_pairing`: `action=status` —
Tier 0 (без подтверждения), `on`/`off` — Tier 1 (`fb_confirm`). Шаг 0 в
`prompts/cluster-setup.md`. Сделано: fbmcp `9b7f189`, `2f3562d`, `4f38301`
(ветка `feat/rcm-cluster-pairing`).

Этап B требует релиза fbagent; без него этап A работает в первые 24 ч или
после ручного `--cluster-pairing on`.

## 5. Проверка на DigitalOcean

Два дроплета Ubuntu 24.04 (fra1, 2 vCPU / 4 GB, private network, облачный
файрвол: SSH только с адреса оператора, всё внутри VPC):

1. `.run` 2027.2.28 fb50 на обоих.
2. Замена `fbmcp` на сборку ветки, `fbmcp ctl hqbird-install` → секция
   `rcm:` и секрет.
3. Через MCP (stdio, локальная личность): `fb_cluster_status` →
   `fb_cluster_rcm_password` → `fb_cluster_probe` → `fb_cluster_link` →
   `fb_cluster_replica_folder` → `fb_cluster_enroll` →
   `fb_cluster_firebird_restart` → `fb_cluster_publication` →
   `fb_cluster_reinit` → `fb_cluster_verify`.
4. Критерий: пара «Ok» в `fb_cluster_status view=databases`, verify `ok`,
   `COUNT(*)` таблиц совпадает.
5. Удалить дроплеты и файрвол по ID.

## 6. Риски и решения

- **Отпечатки при связи.** Человек сравнивает отпечаток из запроса со
  строкой `Pairing:` установщика; исполнитель повторяет probe и
  отказывает, если отпечаток изменился.
- **Пароль RCM.** Только в секрете fbmcp (0600, пользователь сервиса),
  никогда в выводе, аудите или тексте запроса. Если оператор сменил пароль
  в UI: `fbmcp ctl secret set FBMCP_RCM_PASSWORD` (значение из stdin).
- **Окно Tier 2.** Ключ окна — `db` из реестра fbmcp, как у
  `fb_agent_restart`.
- **Реплика.** После связи RCM реплики остановлен; fbmcp реплики
  показывает «RCM не отвечает» с подсказкой, что консоль — RCM мастера.

## 7. Результаты проверки 2026-11-12

Этап A сделан (ветка `feat/rcm-cluster`). Проверено на двух дроплетах DO
(Ubuntu 24.04, кит `hqbird2027-fb50-linux-2027.2.28.run`, мастер `rcm-m`,
реплика `rcm-r`), fbmcp — сборка этой ветки.

- `fbmcp ctl hqbird-install` нашёл RCM: записал секцию `rcm:` и секрет
  `FBMCP_RCM_PASSWORD`.
- Прошли все шаги `prompts/cluster-setup.md`: status, probe (отпечатки
  совпали со строками `Pairing:`), rcm_password, link, replica_folder,
  enroll (dry run в запросе), firebird_restart (Tier 2, approve),
  publication, reinit (Tier 2). Пара `Ok` / `IN_SYNC`, verify — `ok`.
- Найдено и исправлено при проверке: `db` у кластерных инструментов
  теперь необязателен (на новом сервере нет баз); `fbmcp.yaml.prev` после
  `hqbird-install` принадлежал root — теперь пользователю сервиса.
- Не fbmcp: таблица создана после первого reinit, опубликована позже,
  повторный reinit с `remove_old_replica` — реплика `LAGGING`,
  `replication.log`: `Table T_FBMCP already exists`, сегмент 1 после
  reinit применяется повторно. Вопрос к RCM/Firebird (hqcluster3).
- В команде RCM поле `actor` = `rcmadmin`: заголовок `X-Admin-User` в
  `actor` команды не попадает (аудит fbmcp пишет своего пользователя).
- Этап B на этой проверке ещё не был сделан (см. ниже).

### Этап B, проверка 2026-11-12

Два дроплета DO (Ubuntu 24.04, кит 2027.2.28 fb50 `--yes`, `pair-m` и
`pair-r`), fbagent и fbmcp заменены сборками веток этапа B (fbagent
2.999.0), затем `fbmcp ctl hqbird-install`. MCP stdio, локальная личность.

- `fb_agent_cluster_pairing` (status) на обоих: окно открыто установщиком,
  отпечаток совпал со строкой `Pairing:`.
- `action=off` → запрос Tier 1 → `fb_confirm`: окно закрыто, через минуту
  порт 7450 не слушается. `fb_cluster_probe` с мастера: реплика
  `error unreachable` (connection refused).
- `action=on hours=2` → `fb_confirm` на обоих: окно до +2 ч, порт 7450
  открыт, `fbagent --cluster-pairing status` показывает то же.
  `fb_cluster_probe`: реплика `state free`, fbagent 2.999.0, отпечаток
  совпал с выводом установщика.
- Аудит fbagent: `POST /v1/cluster/pairing`, `requested_by` = `local`,
  `reason` из запроса. Аудит fbmcp: pending → approved (in-band-token).
- Дроплеты и файрвол удалены по ID.

Этап B требует релиза fbagent (capability `cluster_pairing`); со старым
fbagent инструмент отвечает `not-found` и подсказывает команду CLI.

## 8. Дальше

Что сделано, что поменялось в релизе 2027.2.29 и план этапов C и D:
[rcm-cluster-next.ru.md](rcm-cluster-next.ru.md).
