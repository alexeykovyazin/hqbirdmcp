# fbmcp и HQbird RCM: что сделано и что дальше (2026-11-13)

Цель: поднять RCM, связать узлы и запустить кластер командами fbmcp.
Первый план и результаты проверки: [rcm-cluster-plan.ru.md](rcm-cluster-plan.ru.md).

## 1. Что сделано

Всё ниже вышло в релизе **2027.2.29** (закрыт 2026-11-13): fbagent 3.1.0,
HQ Monitor 1.54.0, node и RCM 2027.5.9, fbmcp 2027.2.22, setups 2027.2.29,
стек goafts 1.19.0.

| Работа | Коммиты | В релизе |
|---|---|---|
| Этап A: 10 инструментов `fb_cluster_*`, секция `rcm:` и секрет в `fbmcp ctl hqbird-install`, playbook `prompts/cluster-setup.md` | fbmcp `f94a204`..`ad8ba89` | fbmcp 2027.2.22 |
| Этап B: окно pairing через агент (`GET/POST /v1/cluster/pairing`, `fb_agent_cluster_pairing`) | fbagent `e6e1b06d`, `883ef67d`; fbmcp `9b7f189`..`267b53a` | fbagent 3.1.0, fbmcp 2027.2.22 |
| Узел не повторяет DDL, который уже есть в образе (LAGGING после 2-го reinit) | node `7184803` | node 2027.5.9 |
| Go 1.26.11 (GO-2026-6859), CI берёт версию Go из `go.mod` | node `b2ada26`, `9b6c311`; RCM `3a9a7b6`, `c99f461` | node и RCM 2027.5.9 |
| Консоль RCM: браузер в UTC показывает «UTC», не «UTC+0» | RCM `9bc767a` | RCM 2027.5.9 |

Проверка на дроплетах DO (кит 2027.2.28, 2026-11-12): этапы A и B прошли,
пара `Ok` / `IN_SYNC`, verify `ok`. Ветка `feat/rcm-cluster` влита в `master`.

## 2. Что поменялось рядом (другие работы, тоже в 2027.2.29)

- **RCM необязателен.** Установка по умолчанию: без RCM и без окна pairing.
  Флаги кита: `--rcm on` (мастер), `--cluster-pairing on` (реплика),
  `--pairing-port` (слот 7450-7459).
- **fbagent 3.1.0, local API:** `GET /v1/cluster` (состояние),
  `POST /v1/cluster/rcm` (создать RCM на хосте, операция; пароль только в
  файле на хосте), `/v1/cluster/admin/<route>` (admin API RCM этого хоста
  через агент, с сертификатом подписанта, без пароля RCM). Capabilities:
  `cluster`, `cluster_rcm`, `cluster_admin`.
- **Слоты pairing 7450-7459:** у каждого агента свой порт. В ответе окна
  есть поле `firewall`.
- **RCM 2027.5.9, probe:** строка на агента (`host:port`), поля
  `pairing_port`, `node_ports`, `firebird_port`, `instance`, `skipped`
  (порты `closed`, `not_agent`, `contract`).
- **RCM 2027.5.9, link:** поля `new_master` и `replica_of`. Одна свободная
  строка в кластере, где уже есть мастер, → отказ `lone_master`.
- **Выход из кластера:** `fbagent --cluster-leave`, авто-выход, выход при
  удалении. Только CLI, маршрута в local API нет.
- **HQ Monitor 1.54.0:** страница Clusters через агент.

## 3. Разрывы fbmcp 2027.2.22

| # | Разрыв | Следствие |
|---|---|---|
| G1 | Нет команды fbmcp «создать RCM» | На ките 2027.2.29 по умолчанию RCM нет → `hqbird-install` не пишет `rcm:` → `fb_cluster_*` скрыты. Цель «поднять RCM через fbmcp» не выполнена |
| G2 | `fb_cluster_link` без `new_master` / `replica_of` | Новая реплика к готовому кластеру (одна строка) → `lone_master` |
| G3 | `fb_cluster_probe` не показывает поля агента и `skipped` | Два экземпляра на хосте не различить. Проверить: повторный probe перед link сверяет отпечаток по `host:port`, а не по хосту |
| G4 | RCM только через Digest и секрет `FBMCP_RCM_PASSWORD` | После `POST /v1/cluster/rcm` нужен повторный `sudo fbmcp ctl hqbird-install`. Прокси агента (`/v1/cluster/admin`) в RCM 2027.5.9 пропускает только `/clusters` и `/agents`: probe, link, nodes, enroll, reinit, verify через него не идут |
| G5 | В команде RCM `actor` = `rcmadmin` | Остаётся: RCM пишет `X-Admin-User` только в аудит admin-маршрутов |
| G6 | Тексты `fb_agent_cluster_pairing` говорят «порт 7450» | Слот может быть 7451-7459; поле `firewall` не показано |
| G7 | Нет выхода из кластера и удаления члена | Только CLI fbagent и консоль RCM |
| G8 | Playbook описывает порядок кита 2027.2.28 | Не подходит для установки по умолчанию 2027.2.29 |

## 4. План

### Этап C. Кластер с нуля на ките 2027.2.29 (fbmcp 2027.2.23)

- **C1.** fbmcp `agentapi`: `GET /v1/cluster`, `POST /v1/cluster/rcm`
  (операция). Новые инструменты:
  - `fb_agent_cluster` — Tier 0: кластер агента, RCM хоста, слот, окно.
  - `fb_agent_cluster_rcm` — Tier 1 (`fb_confirm`): создать RCM на этом
    хосте, ждать операцию. Вывод: `cluster_id`, `rcm_url`, `console_url`,
    `ca_hash`, путь к файлу пароля. Пароля в выводе нет.
- **C2.** Транспорт RCM не меняется: Digest и секрет `FBMCP_RCM_PASSWORD`
  (решение R1 = A). `fb_agent_cluster_rcm` в конце говорит следующий шаг:
  `sudo fbmcp ctl hqbird-install` на этом хосте (секция `rcm:` и секрет из
  `clusterca/<id>/rcm-initial-password`), потом переподключить MCP-клиент.
- **C3.** Видимость `fb_cluster_*` — как раньше, по секции `rcm:`.
  `fb_agent_cluster` подсказывает `hqbird-install`, когда RCM на хосте есть,
  а секции нет.
- **C4.** `fb_cluster_link`: параметры `new_master`, `replica_of`. Ошибка
  `lone_master` → подсказка с обоими вариантами.
- **C5.** `fb_cluster_probe`: строки по агентам (`host:port`, `instance`,
  `pairing_port`, `firebird_port`, `node_ports`) и `skipped`. Исполнитель
  link сверяет отпечаток по строке.
- **C6.** `fb_agent_cluster_pairing`: порт из ответа (слот), поле
  `firewall`, тексты без «7450».
- **C7.** Playbook `prompts/cluster-setup.md` и справочник, новый порядок:
  1. Оба хоста: кит 2027.2.29 без флагов, `fbmcp ctl hqbird-install`.
  2. Мастер: `fb_agent_cluster` → `fb_agent_cluster_rcm` →
     `sudo fbmcp ctl hqbird-install` → переподключение →
     `fb_cluster_rcm_password`.
  3. Реплика: `fb_agent_cluster_pairing action=on hours=2`.
  4. Мастер: `fb_cluster_probe` → `fb_cluster_link` (две строки; для
     новой реплики — `replica_of`).
  5. `fb_cluster_replica_folder` → `fb_cluster_enroll` →
     `fb_cluster_firebird_restart` → `fb_cluster_publication` →
     `fb_cluster_reinit` → `fb_cluster_verify`.
  6. Реплика: `fb_agent_cluster_pairing action=off`.
- **C8.** Тесты: unit (`agentapi`, параметры link, probe, гейт), `go vet`,
  `gofmt`, `govulncheck`. Go 1.26.11 в fbmcp (T2).
- **C9.** Проверка на DO: 2 дроплета, кит 2027.2.29 с ib-aid.com, сборка
  fbmcp ветки. Весь порядок C7 с нуля; по SSH только установка кита и
  `fbmcp ctl hqbird-install`. Критерии: RCM создан через fbmcp, пара `Ok` /
  `IN_SYNC`, verify `ok`.
  Доп. случаи: отказ `lone_master` и третий узел через `replica_of`.
  Удалить дроплеты и файрвол по ID.
- **C10.** fbmcp 2027.2.23 — в следующий релиз setups (`hqbird-release`).

### Этап D. Выход из кластера через fbmcp (fbagent + fbmcp)

- **D1.** fbagent: `POST /v1/cluster/leave` (операция, как
  `--cluster-leave`), capability `cluster_leave`. Нужен релиз fbagent.
- **D2.** fbmcp: `fb_agent_cluster_leave` (Tier 2: останавливает службы
  узла). Удаление члена через admin API RCM — проверить, есть ли маршрут.
- **D3.** Проверка на DO: реплика выходит, потом связывается снова.

### Хвосты вне fbmcp

- **T1.** CI RCM: задача `contract` красная — нет секрета
  `NODE_REPO_TOKEN` в `alexeykovyazin/hqcluster3` (fine-grained PAT, только
  чтение `hqcluster-node`). Добавляет владелец. Потом
  `gh workflow run ci -R alexeykovyazin/hqcluster3 --ref main` (на main есть
  коммиты без прогона CI).
- **T2.** Go 1.26.11 в других репо: fbmcp (`go 1.25.13`, в ветке 1.25
  исправления нет; 2027.2.22 собран с ней), goafts (`go1.26.10`, стек
  1.19.0), hqbirdmonitor (`go 1.26.8`, 1.54.0), fblogspacker (`go 1.25`).
  Шаги: `govulncheck` → `toolchain go1.26.11` → CI `go-version-file` →
  следующий релиз. fbmcp — первым, вместе с этапом C.
- **T3.** node main `fa02184` (сегмент, который Firebird ещё пишет, не
  применяется) не выпущен. Идёт в следующий релиз node и RCM (одна версия).

## 5. Решения владельца (2026-11-13)

- **R1 = A.** Транспорт RCM — Digest и пароль, RCM не меняем. Прокси
  агента не подходит: в RCM 2027.5.9 он пропускает только `/clusters` и
  `/agents`. Расширение прокси в RCM (вариант B) отклонено.
- **R2 = Tier 1.** `fb_agent_cluster_rcm` — Tier 1 (`fb_confirm`): Firebird
  не останавливается.
- **R3 = да.** Этап D — после этапа C, с отдельным релизом fbagent.

## 6. Порядок

1. T1 (владелец).
2. Этап C (C1-C8) → проверка C9 → C10 в следующем релизе.
3. T2 — в тот же релиз (fbmcp первым).
4. Этап D — после этапа C.
