# work9flow

Local-first workflow orchestrator. Go TUI + runtime, built on top of [DeepSeek Harness](https://github.com/deepseek-ai).

`work9flow` — это **workflow runtime**, а не отдельный кодинг-агент. Он координирует цепочку специализированных агентов (Repository Scout → Planner → Plan Gatekeeper → Implementer → Review workers) поверх версионируемого протокола артефактов, с human-in-the-loop, durable state и live-наблюдаемостью.

Первый workflow в MVP — `feature-development`:

```
repo discovery → planning → plan review / user clarification → implementation
                → parallel review → correction loop → done
```

## Что будет в MVP

- **`work9flowd`** — durable workflow controller: state machine, registry, persistence, DSH-адаптер, нормализация событий, Attention-сервис.
- **Workflow `feature-development`** целиком: итерационные лимиты, parallel review fan-out/fan-in, детерминированная маршрутизация findings, терминальные `DONE` / `FAILED` / `CANCELED`.
- **Go TUI** — тонкий клиент для live-супервайзинга, инспекции агентов, просмотра артефактов, ответов на Attention, `steer` / `followup`.
- **Локальный протокол** — localhost HTTP + WebSocket для событий. Типы DSH изолированы за адаптером.
- **Persistence** — local-first; SQLite как сильный дефолт, если конвенции репо не диктуют иное.

Post-MVP (Web UI, IDE-интеграции, multi-user remote, дополнительные workflows, marketplace) — отдельный эпик, не расширяет MVP-скоуп.

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

## Архитектура

```
                  ┌──────────────────────────────┐
                  │        work9flow TUI         │
                  │  (Go • thin client only)     │
                  └──────────────┬───────────────┘
                                 │  local HTTP + WebSocket
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
                              │  adapter interface
                              ▼
                   ┌─────────────────────┐
                   │  DeepSeek Harness   │
                   │  (agent runtime,    │
                   │   tools, sessions)  │
                   └──────────┬──────────┘
                              ▼
                   ┌─────────────────────┐
                   │  Model providers    │
                   └─────────────────────┘
```

**Правила владения**

| Слой | Владеет | Не владеет |
|---|---|---|
| `work9flowd` | workflow state, registry, events, artifacts, Attention lifecycle, version history | internals агентов |
| DeepSeek Harness | agent sessions, model/tool execution, subagents | workflow state |
| TUI | рендеринг, пользовательский ввод | любым persistent state |
| DSH-адаптер | нормализация событий, изоляция DSH-типов | workflow business logic |

**Архитектурные границы**

* `work9flowd` (`cmd/work9flowd/`) — runtime/контроллер: state machine, registry, persistence, DSH-адаптер, артефакты, Attention, нормализация событий.
* DeepSeek Harness — ядро исполнения агентов/моделей/сессий/сабагентов.
* work9flow TUI (планируется `cmd/work9flow/`) — тонкий клиент. Подписывается на протокол, рисует state/events, шлёт команды. Закрытие TUI не останавливает run.

TUI не импортирует пакеты DeepSeek Harness и не дублирует семантику DSH-событий.

## Роли агентов в `feature-development`

1. **Repository Scout** — read-only сбор evidence. Артефакты: `breadcrumbs.json`, `repository-map.md`, `sources.json`, `skills.json`. Никогда не правит production-код.
2. **Planner / Architect** — производит версионируемые `feature-spec.md` и `implementation-plan.md`. Смотрит git history, чтобы не считать транзитный код финальной архитектурой.
3. **Plan Gatekeeper** — независимое ревью. Outcomes: `approved`, `revise_plan`, `user_input_required`. Сам плана не переписывает.
4. **Implementer** — исполняет одобренный план; эмитит структурированный result-метаданные.
5. **Review orchestrator + workers** — параллельный fan-out (correctness, architecture, tests, scope); синтезированные findings маршрутизируются политикой контроллера, а не ad-hoc выбором агента.

**Артефакты — это протокол между агентами.** Каждая стадия получает только те явные данные, что ей нужны, из durable state плюс версионируемых артефактов. Оригинальная задача иммутабельна; clarifications — append-only.

## Структура репозитория (план)

Финальная раскладка закрепляется в MVP 01. Рабочий набросок:

```
work9flow/
├── cmd/
│   ├── work9flowd/        # runtime / контроллер
│   └── work9flow/         # Go TUI клиент
├── internal/
│   ├── workflow/          # state machine + registry
│   ├── attention/         # QUESTION / DECISION / APPROVAL
│   ├── artifacts/         # versioned store
│   ├── persistence/       # SQLite + event log
│   ├── dsh/               # DeepSeek Harness adapter
│   ├── protocol/          # local client API + event schema
│   └── tui/               # Go TUI client internals
├── docs/
│   ├── architecture.md
│   └── workflow-feature-development.md
└── examples/
    └── feature-development/
```

## Статус

Сейчас: **бутстрап MVP 01**. Реализовано:
- Go-модуль `github.com/unbot/work9flow`;
- прямые зависимости Charm + Catppuccin зафиксированы в `go.mod`;
- entrypoint `cmd/work9flowd/` с логированием через `charm/log`;
- beads-трекинг для MVP 01 → MVP 07.

Дальше — runtime (MVP 02), state machine (MVP 04), DSH-адаптер (MVP 03), TUI (MVP 07).

Дорожная карта в [Beads]:

| Beads ID | Title |
|---|---|
| `work9flow-19x` | epic — work9flow MVP |
| `work9flow-19x.1` | MVP 01 — Bootstrap runtime & lock architecture |
| `work9flow-19x.7` | MVP 02 — Protocol, domain model, persistence |
| `work9flow-19x.2` | MVP 03 — DSH adapter |
| `work9flow-19x.5` | MVP 04 — Workflow engine & Attention |
| `work9flow-19x.3` | MVP 05 — Discovery / planning / plan review |
| `work9flow-19x.6` | MVP 06 — Implementation / review |
| `work9flow-19x.4` | MVP 07 — Go TUI |
| `work9flow-sre` | epic — Post-MVP extensions (blocked on MVP) |

Beads — источник истины по статусам. `bd ready` — следующая разблокированная задача, `bd show <id>` — полное описание.

## Почему отдельный runtime?

LLM рассуждает. Обычный код владеет state, routing, iteration limits, permissions, versioning и invariants. Standalone-рантайм делает workflow durable, restartable, replayable и инспектируемым — независимо от конкретного агента или терминальной сессии.
