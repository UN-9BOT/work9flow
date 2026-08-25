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
make smoke         # boot runtime + exercise endpoints + TUI --once
make healthcheck   # non-interactive work9flow --once
make run-runtime   # запустить work9flowd на 127.0.0.1:7469
```

State dir по умолчанию: `~/.local/state/work9flow` (`XDG_STATE_HOME/work9flow`).
SQLite database: `<state_dir>/work9flow.db`.

## Конфигурация

1. `Defaults()` (`http://127.0.0.1:7469`, XDG state dir).
2. YAML: `--config work9flow.yaml`.
3. ENV: `WORK9FLOW_STATE_DIR`, `WORK9FLOW_RUNTIME_ENDPOINT`, `WORK9FLOW_DSH_ENDPOINT`, `WORK9FLOW_WORKSPACE_DIR`.

## Статус

MVP 01 (BIR-57) и MVP 02 (BIR-58) закрыты. Контракт + домен + persistence + WS event
stream работают; проверено `make test` (49+ PASS), `make smoke`. Дальше —
[BIR-59 → BIR-63](.beads/issues.jsonl): DSH integration, state machine,
discovery/planning/review, execution, TUI screens.
