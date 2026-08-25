# work9flow — Architecture (MVP 01)

Этот документ фиксирует процессные границы и направление зависимостей. Любой
contributor, меняющий структуру пакетов или контракт между runtime и клиентом,
обязан сначала обновить его и согласовать.

## Процессы

```
┌──────────────────┐        HTTP/JSON           ┌──────────────────┐
│   work9flow      │ ─────────────────────────▶ │  work9flowd      │
│   (TUI client)   │ ◀───────────────────────── │  (runtime)       │
│   cmd/workflow   │   GET /v1/health, etc.     │  cmd/workflowd   │
└──────────────────┘                            └────────┬─────────┘
                                                          │ HTTP/JSON
                                                          ▼
                                                 ┌──────────────────┐
                                                 │  DSH (Node)      │
                                                 │  DeepSeek        │
                                                 │  Harness         │
                                                 └──────────────────┘
```

* `work9flowd` — долгоживущий runtime. Владеет state machine workflow,
  registry, persistence, артефактами, Attention и нормализацией событий.
* `work9flow` (TUI) — коротко- или долгоживущий клиент. Подписывается на
  протокол, рисует state/events, шлёт команды. **Закрытие TUI не останавливает
  ни один run.**
* DeepSeek Harness (DSH) — отдельный Node-процесс. Go-runtime общается с ним
  по HTTP/JSON через `internal/dsh`. У DSH нет Go-SDK, поэтому протечь DSH-типы
  в Go физически не могут.

## Транспорт

MVP transport: **HTTP/1.1 + JSON** на `127.0.0.1` (default `http://127.0.0.1:7469`).

Контракт живёт в `internal/protocol` и ограничен набором DTO:

| Метод | Путь | Ответ |
| --- | --- | --- |
| GET  | `/v1/health`  | `HealthResponse` |
| GET  | `/v1/version` | `VersionResponse` |
| GET  | `/v1/runs`    | `RunListResponse` (empty на MVP 01) |

Все ответы — `application/json`. 404 тоже JSON: `{"error":"not_found"}`.

Event stream (run/session/attention events) появится в MVP 02 вместе с
WebSocket-эндпоинтом. До этого момента клиент poll'ит `/v1/health` каждые 5с.

## Направление зависимостей

```
cmd/work9flow   ──┐
                  ├──▶ internal/protocol
cmd/work9flowd  ──┤
                  ├──▶ internal/runtime  ──▶ internal/protocol
                  ├──▶ internal/config
                  └──▶ internal/dsh      ──▶ (HTTP к DSH)
```

Правила:

1. `internal/protocol` не импортирует ничего из `internal/runtime`,
   `internal/dsh` или `internal/config`. Это публичный wire-контракт.
2. `cmd/work9flow` (TUI) **не импортирует** `internal/runtime`, `internal/dsh`
   и тем более DSH-типы. Только `internal/protocol` + `internal/config`.
3. `internal/dsh` не импортирует `internal/runtime` и не возвращает DSH-сырые
   структуры — всё маппится в типы `internal/protocol`.
4. Никакой пакет не импортирует `cmd/*` — `cmd/` это executables.

Это и есть гарантия "DSH-типы не протекают в Go client contract".

## Конфигурация

Загрузка: `internal/config.Load(path)`.

Источники в порядке возрастания приоритета:

1. `Defaults()` (XDG-compliant `state_dir`, `http://127.0.0.1:7469`).
2. YAML-файл по `--config` (или `WORK9FLOW_CONFIG`).
3. ENV: `WORK9FLOW_STATE_DIR`, `WORK9FLOW_RUNTIME_ENDPOINT`,
   `WORK9FLOW_DSH_ENDPOINT`, `WORK9FLOW_WORKSPACE_DIR`.

Схема в `Config` (`internal/config/config.go`).

## Логирование

`charmbracelet/log` (stderr, level по умолчанию — `info`). TUI выставляет
`warn`, чтобы не спамить в окно во время alt-screen.

## Текущий статус MVP 01

Сделано:

* ✅ `go.mod` со стеком Charm + Catppuccin (см. README).
* ✅ `internal/config` (YAML/env, defaults, validate, 6 тестов).
* ✅ `internal/protocol` (DTO + JSON round-trip, 3 теста).
* ✅ `internal/runtime` (HTTP server, `/v1/health` + `/v1/version` + `/v1/runs`,
  graceful shutdown, JSON 404, 5 тестов).
* ✅ `internal/dsh` (HTTP-клиент к DSH, `Health` + `CreateSession`, 3 теста).
* ✅ `cmd/work9flowd` (config → runtime → DSH probe → signal-aware shutdown).
* ✅ `cmd/work9flow` (TUI client: bubbletea + lipgloss + catppuccin; poll
  health; `q` отключает TUI, не останавливая runtime; `--once` для CI).
* ✅ Makefile + smoke.

Не сделано (явно out-of-scope MVP 01):

* ❌ Workflow state machine, stages, Attention — MVP 04.
* ❌ Domain types `Workflow/WorkflowRun/AgentRun/Artifact/...` — MVP 02.
* ❌ Persistence (event log, durable state) — MVP 02.
* ❌ WebSocket event stream — MVP 02.
* ❌ Реальная интеграция с DSH (только probe) — MVP 03.
* ❌ Богатые TUI-экраны — MVP 07.
