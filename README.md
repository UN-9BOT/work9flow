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
make smoke-full    # boot inline DSH + scripted provider, drive run → DONE
make healthcheck   # non-interactive work9flow --once
make run-runtime   # запустить work9flowd на 127.0.0.1:7469
```

State dir по умолчанию: `~/.local/state/work9flow` (`XDG_STATE_HOME/work9flow`).
SQLite database: `<state_dir>/work9flow.db`.

## Конфигурация

1. `Defaults()` (`http://127.0.0.1:7469`, XDG state dir).
2. YAML: `--config work9flow.yaml`.
3. ENV: `WORK9FLOW_STATE_DIR`, `WORK9FLOW_RUNTIME_ENDPOINT`, `WORK9FLOW_DSH_ENDPOINT`, `WORK9FLOW_WORKSPACE_DIR`, `WORK9FLOW_PROVIDERS_FILE`.

`work9flow.yaml` пример:

```yaml
state_dir: ~/.local/state/work9flow
runtime_endpoint: http://127.0.0.1:7469
# dsh_endpoint: http://127.0.0.1:7770   # опционально: внешний DSH (Node)
providers_file: ./providers.toml         # альтернана — поднять inline DSH
iteration_limits:
  default: 5
  implementing: 3
model_roles:
  default: minim/MiniMax-M3
```

## Провайдеры (`providers.toml`)

`providers.toml` описывает LLM-провайдеров с OpenAI-совместимым API.
Файл подгружается при старте `work9flowd`, когда `dsh_endpoint` пуст —
демон сам поднимает маленький DSH-совместимый HTTP-сервер, который
перенаправляет сессии в указанный провайдер. Это позволяет гонять
полный feature-development pipeline (scout → planner → gatekeeper →
implementer → reviewer) без внешнего Node-процесса DSH.

```toml
[minim]
display_name = "Custom (minim)"
protocol     = "openai"
base_url     = "https://api.minimax.io/v1"
api_key_env  = "MINIM_API_KEY"
default_model = "minim/MiniMax-M3"

[[minim.models]]
id = "MiniMax-M3"
tier = "strong"
context_window = 400000
max_output_tokens = 131072
supports_thinking = true
supports_vision = true
```

Запуск с реальным minim: `export MINIM_API_KEY=... && work9flowd --config=work9flow.yaml`.

## Статус

MVP 01–07 закрыты (см. `bd list` / `git log --oneline`). CRUD-слой,
state machine, DSH-адаптер, агенты (scout/planner/gatekeeper/implementer/reviewer),
TUI, inline OpenAI-провайдер и `minim` зарегистрированы. Полный pipeline
end-to-end проверяется через `make smoke-full` (run доходит до DONE через
inline DSH + scripted OpenAI-провайдер). Реальный запуск с `minim` —
`export MINIM_API_KEY=... && work9flowd --config=work9flow.yaml`.
