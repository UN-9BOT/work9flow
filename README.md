# work9flow

Local-first workflow orchestrator. Go TUI + runtime, built on top of [DeepSeek Harness](https://github.com/deepseek-ai).

## Stack

UI и протокольный слой построены на экосистеме [Charm](https://github.com/charmbracelet) + палитре [Catppuccin](https://github.com/catppuccin/go).

| Библиотека | Версия | Для чего |
| --- | --- | --- |
| [`charmbracelet/bubbletea`](https://github.com/charmbracelet/bubbletea) | v1.3.10 | TUI-фреймворк (Elm-архитектура: `Model` / `Update` / `View`). Основа всех экранов work9flow: дашборд runов, инспектор агента, диалоги Attention. |
| [`charmbracelet/lipgloss`](https://github.com/charmbracelet/lipgloss) | v1.1.1 | Декларативные стили и layout TUI: рамки, отступы, темы, выравнивание. Поверх палитры Catppuccin. |
| [`charmbracelet/huh`](https://github.com/charmbracelet/huh) | v1.0.0 | Интерактивные формы и промпты поверх `bubbletea`. Старт нового workflow-run, ответы на Attention-вопросы, ввод параметров. |
| [`charmbracelet/glamour`](https://github.com/charmbracelet/glamour) | v1.0.0 | Рендер Markdown в терминал с темами (включая Catppuccin). Показ планов, отчётов, логов и артефактов workflow прямо в TUI. |
| [`charmbracelet/log`](https://github.com/charmbracelet/log) | v1.0.0 | Структурированный логгер: уровни, JSON-вывод, программируемые хендлеры. Логи `work9flowd` и самого TUI. |
| [`catppuccin/go`](https://pkg.go.dev/github.com/catppuccin/go) | v0.3.0 | Палитра Catppuccin (`Latte`/`Frappé`/`Macchiato`/`Mocha`). Источник цветов для `lipgloss`-стилей и подсветки статусов. |

> **Примечание:** `charmbracelet/glow` — это CLI-приложение для просмотра Markdown в TUI, не библиотека. Его рендер-движок — `glamour`, который мы и используем. Если в будущем понадобится просматривать артефакты workflow вне TUI, можно будет shell-вызывать бинарь `glow`.

## Архитектурные границы

```
   ┌──────────────────┐  HTTP/JSON  ┌──────────────────┐
   │ work9flow (TUI)  │ ──────────▶ │ work9flowd       │ ──▶ DSH (Node)
   │ cmd/work9flow    │ ◀────────── │ cmd/work9flowd   │     через internal/dsh
   └──────────────────┘             └──────────────────┘
```

* `work9flowd` — долгоживущий runtime. Владеет state machine, persistence, артефактами, Attention, нормализацией событий. В MVP 01 отдаёт только `/v1/health`, `/v1/version`, `/v1/runs` (пусто).
* DeepSeek Harness (DSH) — Node-процесс. У DSH нет Go-SDK, поэтому `internal/dsh` — единственная Go-граница с DSH, и DSH-типы физически не могут протечь в TUI.
* work9flow TUI (`cmd/work9flow`) — тонкий клиент. Подписывается на протокол, рисует state/events, шлёт команды. **Закрытие TUI не останавливает run.**

TUI не импортирует `internal/runtime`, `internal/dsh` и DSH-типы. Только `internal/protocol` + `internal/config`. Подробности — в [ARCHITECTURE.md](ARCHITECTURE.md).

## Структура

```
cmd/
  work9flowd/    runtime entrypoint (config → HTTP server → DSH probe)
  work9flow/     TUI client (bubbletea + lipgloss + catppuccin)
internal/
  config/        YAML+env loader, defaults, validation
  protocol/      JSON DTOs shared between runtime and clients
  runtime/       HTTP server, handlers, graceful shutdown
  dsh/           HTTP client to DSH (only package that knows DSH)
scripts/
  smoke.sh       boot runtime, hit endpoints, verify
Makefile         build / test / vet / run-runtime / smoke / healthcheck
ARCHITECTURE.md  процессные границы и направление зависимостей
```

## Команды

```bash
make build       # бинари в ./bin
make test        # go test ./...
make vet         # go vet ./...
make run-runtime # запустить work9flowd на 127.0.0.1:7469
make smoke       # поднять runtime, дёрнуть /v1/{health,version,runs}, TUI --once
make healthcheck # неинтерактивный вызов work9flow --once
make tidy        # go mod tidy
```

## Конфигурация

Загрузка: `internal/config.Load(--config path)`. Источники по приоритету:

1. `Defaults()` (`http://127.0.0.1:7469`, XDG state dir).
2. YAML: `--config work9flow.yaml`.
3. ENV: `WORK9FLOW_STATE_DIR`, `WORK9FLOW_RUNTIME_ENDPOINT`, `WORK9FLOW_DSH_ENDPOINT`, `WORK9FLOW_WORKSPACE_DIR`.

## Статус

MVP 01 закрыт. Bootstrap готов: runtime + TUI отдельные процессы, контракт HTTP+JSON,
health/version работают, TUI poll'ит runtime, доказано скриптом `make smoke`.
Дальше — [MVP 02 → MVP 07](.beads/issues.jsonl) (домен + persistence + WebSocket + DSH + TUI-экраны).
