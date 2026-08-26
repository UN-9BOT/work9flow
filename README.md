# work9flow

Local-first workflow orchestrator. Go TUI + runtime, built on top of [DeepSeek Harness](https://github.com/deepseek-ai).

## Stack

UI и протокольный слой построены на экосистеме [Charm](https://github.com/charmbracelet) + палитре [Catppuccin](https://github.com/catppuccin/go).

| Библиотека | Версия | Зачем |
| --- | --- | --- |
| [`catppuccin/go`](https://github.com/catppuccin/go) | v0.3.0 | палитра (Mocha/Frappe/Latte/Macchiato) для TUI и runtime |
| [`charmbracelet/lipgloss`](https://github.com/charmbracelet/lipgloss) | v1.1.1 | стили, layout, рендер цветного текста в TUI |
| [`charmbracelet/bubbletea`](https://github.com/charmbracelet/bubbletea) | v1.3.10 | Elm-style TUI framework (Model/Update/View) |
| [`charmbracelet/glamour`](https://github.com/charmbracelet/glamour) | v1.0.0 | markdown renderer для артефактов (feature-spec, plans) |
| [`charmbracelet/huh`](https://github.com/charmbracelet/huh) | v1.0.0 | формы/промпты для Attention (QUESTION/DECISION/APPROVAL) |
| [`charmbracelet/log`](https://github.com/charmbracelet/log) | v1.0.0 | structured logging (runtime info, TUI warn) |
| [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) | v1.34.5 | pure-Go SQLite (runtime persistence; no CGO) |
| [`github.com/coder/websocket`](https://github.com/coder/websocket) | v1.8.13 | live event subscription transport |
| `gopkg.in/yaml.v3` | v3.0.1 | YAML config parsing |

> **Примечание:** `charmbracelet/glow` — это CLI-приложение для просмотра Markdown в TUI, не библиотека. Его рендер-движок — `glamour`, который мы и используем.

## Архитектурные границы

```
                  ┌──────────────────────────────┐
                  │        work9flow TUI         │
                  │  (Go • thin client only)     │
                  └──────────────┬───────────────┘
                                 │  HTTP/JSON + WebSocket
                                 ▼
   ┌─────────────────────────────────────────────────────────┐
   │                       work9flowd                        │
   │                                                         │
   │   ┌────────────┐  ┌────────────┐  ┌────────────┐        │
   │   │  workflow  │  │ attention  │  │ artifacts  │        │
   │   │  engine    │  │  service   │  │   store    │        │
   │   └─────┬──────┘  └─────┬──────┘  └─────┬──────┘        │
   │         └───────────────┼───────────────┘               │
   │                  ┌──────┴──────┐                        │
   │                  │  DSH adapter│                        │
   │                  └──────┬──────┘                        │
   │                  ┌──────┴──────┐                        │
   │                  │ persistence │  ◀── SQLite / event log│
   │                  └─────────────┘                        │
   └──────────────────────────┬──────────────────────────────┘
                              │ HTTP/JSON
                              ▼
                     ┌──────────────────┐
                     │   DSH (Node)     │
                     │  DeepSeek Harness│
                     └──────────────────┘
```

Подробнее — [ARCHITECTURE.md](ARCHITECTURE.md).

## Команды

```
make build         # бинари в ./bin (work9flowd + work9flow)
make test          # go test ./...
make vet           # go vet ./...
make tidy          # go mod tidy
make smoke         # boot runtime + CRUD endpoints + TUI --once
# `make smoke-full` was REMOVED in bead work9flow-8w0 (dsh-A.10e) — see Статус below.
make healthcheck   # non-interactive work9flow --once
make run-runtime   # запустить work9flowd на 127.0.0.1:7469
```

State dir по умолчанию: `~/.local/state/work9flow` (`XDG_STATE_HOME/work9flow`).
SQLite database: `<state_dir>/work9flow.db`.

## Конфигурация

1. `Defaults()` (`http://127.0.0.1:7469`, XDG state dir).
2. YAML: `--config work9flow.yaml`.
3. ENV: `WORK9FLOW_STATE_DIR`, `WORK9FLOW_RUNTIME_ENDPOINT`, `WORK9FLOW_DSH_BRIDGE_ADDR`, `WORK9FLOW_WORKSPACE_DIR`.

`work9flow.yaml` пример:

```yaml
state_dir: ~/.local/state/work9flow
runtime_endpoint: http://127.0.0.1:7469
# dsh_bridge_addr: http://127.0.0.1:7770   # external dsh-bridge (Node)
iteration_limits:
  default: 5
  implementing: 3
```

## LLM-провайдеры

`work9flowd` и `runtime/dsh-bridge` — это **два разных процесса**.
`work9flowd` НЕ запускает dsh-bridge; он только подключается к нему
через `dsh_bridge_addr`. dsh-bridge в свою очередь управляет upstream
DeepSeek Harness runtime'ом и его cordis-каталогом провайдеров (см.
`runtime/dsh-bridge/COMPATIBILITY.md` и `runtime/dsh-bridge/README.md`).

Полный запуск выглядит так:

```bash
# shell A: dsh-bridge + upstream DSH runtime
cd runtime/dsh-bridge
npm install
npm run build
export DSH_RUNTIME_EXE=/path/to/dsh-jsonrpc-agent-pkg-<os>-<arch>
export DSH_CORDIS_CONFIG=/path/to/cordis.yml
npm start -- --port 7770

# shell B: work9flowd Go daemon
export WORK9FLOW_DSH_BRIDGE_ADDR=http://127.0.0.1:7770
./bin/work9flowd --config=work9flow.yaml
```

Какие endpoint'ы поддерживаются, решает upstream DSH cordis-каталог.
`work9flowd` не навязывает свой выбор — он только проксирует JSON-RPC
к dsh-bridge. Любой провайдер, для которого реальная cordis-composition
зарегистрировала соответствующий adapter (например, `minimax` через
`https://api.minimax.io/v1`), работает; неизвестные провайдеры DSH не
создаёт автоматически.

## Статус

MVP 01–07 закрыты (см. `bd list` / `git log --oneline`). CRUD-слой,
state machine, DSH-адаптер, агенты (scout/planner/gatekeeper/implementer/reviewer),
TUI, dsh-bridge зарегистрирован.

**Full real-DSH E2E is NOT yet a passed gate.** `make smoke-full`
был удалён вместе с production localdsh path (bead work9flow-8w0).
Новый assembled real-DSH gate появится в bead `work9flow-7dh`
после того, как будет собран `dsh-jsonrpc-agent` runtime и
`runtime/dsh-bridge` начнёт им управлять end-to-end.
