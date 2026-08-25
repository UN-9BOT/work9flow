# work9flow — Architecture (MVP 02)

Этот документ фиксирует процессные границы и направление зависимостей. Любой
contributor, меняющий структуру пакетов или контракт между runtime и клиентом,
обязан сначала обновить его и согласовать.

## Процессы

```
┌──────────────────┐    HTTP/JSON + WebSocket    ┌──────────────────┐
│   work9flow      │ ─────────────────────────▶  │  work9flowd      │
│   (TUI client)   │ ◀─────────────────────────  │  (runtime)       │
│   cmd/work9flow  │   GET /v1/health, etc.      │  cmd/work9flowd  │
└──────────────────┘                             └────────┬─────────┘
                                                            │ HTTP/JSON
                                                            ▼
                                                 ┌──────────────────┐
                                                 │  DSH (Node)      │
                                                 │  DeepSeek        │
                                                 │  Harness         │
                                                 └──────────────────┘
```

* `work9flowd` — долгоживущий runtime. Владеет:
  * workflow registry (MVP 04);
  * durable state machine + event log (MVP 02 — `internal/storage`, `internal/domain`);
  * артефактами и Attention (MVP 02, MVP 04);
  * нормализацией событий от DSH (MVP 03);
  * publishBroker для WS-доставки (MVP 02).
* `work9flow` (TUI) — клиент. Подписывается на `/v1/runs/{id}/events/stream`,
  рисует state/events, шлёт команды (steer/followup/answer/cancel).
  **Закрытие TUI не останавливает ни один run.**
* DSH — Node-процесс, доступ через `internal/dsh`.

## Транспорт

Localhost. Default `127.0.0.1:7469`. Версия протокола: `0.2.0-mvp02`.

| Метод | Путь | Назначение |
| --- | --- | --- |
| GET    | /v1/health                              | runtime liveness |
| GET    | /v1/version                             | runtime version |
| GET    | /v1/runs                                | list runs |
| POST   | /v1/runs                                | create run (RunCreateRequest) |
| GET    | /v1/runs/{id}                           | run detail (RunDetail) |
| DELETE | /v1/runs/{id}                           | cancel run |
| GET    | /v1/runs/{id}/events?after=N            | events with cursor |
| GET    | /v1/runs/{id}/events/stream             | WebSocket: history + live |
| GET    | /v1/runs/{id}/artifacts                 | all artifact versions |
| GET    | /v1/runs/{id}/attentions                | all attention items |
| POST   | /v1/attentions/{id}/answer              | answer an attention item |
| POST   | /v1/runs/{id}/steer                     | steer active agent |
| POST   | /v1/runs/{id}/followup                  | followup queued |

Все ошибки возвращают JSON: `{"error": "<code>", "message": "<details>"}`.

## Направление зависимостей

```
cmd/work9flow          → internal/protocol + internal/config  (тонкий клиент)
cmd/work9flowd         → internal/{config,protocol,runtime,storage,dsh,domain}
internal/runtime       → internal/protocol, internal/storage, internal/domain
internal/protocol      → internal/domain (только type-conversion; без логики)
internal/storage       → internal/domain
internal/domain        → ∅ (никаких внешних импортов кроме stdlib)
internal/dsh           → (HTTP-клиент; не зависит от runtime/protocol)
```

Запрещено:
* TUI импортирует `internal/runtime`, `internal/dsh`, `internal/storage`;
* `internal/protocol` импортирует `internal/runtime` / `internal/storage`;
* `internal/storage` импортирует `internal/runtime` / `internal/protocol`;
* DSH-типы где-либо за пределами `internal/dsh`.

## Домен (`internal/domain`)

Бизнес-типы. Никакого транспорта/персистентности.

* `Workflow`         — definition (id/name/version/stages/limits/roles)
* `WorkflowRun`      — execution instance (immutable OriginalTask, State, Stage, …)
* `AgentRun`         — single agent invocation
* `Artifact`         — versioned, append-only (RunArtifacts.Active tracks latest)
* `Clarification`    — append-only per-run clarification log (RunClarifications)
* `Attention`        — `QUESTION|DECISION|APPROVAL|NOTIFICATION`, lifecycle OPEN→ANSWERED|CANCELED
* `Event`            — append-only event log entry (EventLog, monotonic Seq/PrevSeq)

State machine: `CanTransition(from, to)` валидирует переходы. Терминальные
состояния (DONE/FAILED/CANCELED) absorbing.

## Storage (`internal/storage`)

`Repo` — единственная граница. Конкретная реализация — `sqliteRepo`
(modernc.org/sqlite, pure-Go, no CGO).

Схема (см. `internal/storage/repo.go`): `runs`, `events`, `artifacts`,
`attentions`, `clarifications`. WAL + foreign_keys ON.

Инварианты enforced at storage layer:
* OriginalTask write-once (CreateRun rejects empty; no UPDATE touches it);
* AddArtifact append-only with monotonic version;
* AppendEvent assigns `Seq = MAX(prev)+1` under a transaction;
* AppendClarification append-only;
* UpdateRunState calls `domain.CanTransition` (rejects invalid moves);
* AnswerAttention calls `domain.CanTransitionAttention` (rejects re-answer).

## Protocol (`internal/protocol`)

DTO + JSON converters. Никаких импортов runtime/storage, только domain.

## Runtime (`internal/runtime`)

* `Server` owns HTTP surface, lifecycle, broker.
* `publishBroker` — per-run pub/sub; handlers that create events call
  `broker.publish(runID, event)` so WS subscribers see live updates.

## Config (`internal/config`)

Defaults → YAML (`--config`) → ENV (`WORK9FLOW_*`).

`state_dir` (default `~/.local/state/work9flow` / `XDG_STATE_HOME/work9flow`)
is where `work9flow.db` lives.

## Логирование

`charmbracelet/log`. runtime=info, TUI=warn (не спамит alt-screen).

## Текущий статус MVP 02

Сделано:

* ✅ `internal/domain` (11 типов + CanTransition + tests, 11 PASS).
* ✅ `internal/storage` (SQLite via modernc.org/sqlite, Repo + 16 tests PASS).
* ✅ `internal/protocol` (RunDetail + DTO + converters + tests).
* ✅ `internal/runtime` (handlers + WS broker + 15 tests PASS).
* ✅ `cmd/work9flowd` (config → repo → runtime → DSH probe → signal-aware shutdown).
* ✅ `cmd/work9flow` (TUI клиент: bubbletea + lipgloss + catppuccin, poll health, --once).
* ✅ `scripts/smoke.sh` (boot + create run + steer + cancel + TUI --once).

Не сделано (явно out-of-scope MVP 02):

* ❌ Workflow registry + drivers — MVP 04.
* ❌ Agent run execution + tool plumbing — MVP 04/MVP 06.
* ❌ Artifact planning loop (feature-spec.vN / implementation-plan.vN) — MVP 05.
* ❌ TUI-экраны поверх huh/glamour — MVP 07.

Всего тестов: 49+ PASS (config 6, domain 11, dsh 3, protocol 3, runtime 15, storage 16).
`go vet ./...` чисто.
