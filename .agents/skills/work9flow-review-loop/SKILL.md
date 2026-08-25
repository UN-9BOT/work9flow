---
name: work9flow-review-loop
description: Coordinate a work9flow contributor loop that takes work from `bd`, pushes branches to GitHub, opens a PR, and runs reviewer-ratified code review through ChatGPT via the chrome-devtools-cli skill. Local-only knowledge; not for the public repo.
allowed-tools: Bash(bash *)
---

# work9flow review loop

bead → work → commit → push → PR → chrome-devtools review request →
fix P1 → re-request → approve → merge → next bead.

## Git authority notice (read first)

This skill does **not** grant git-write authority on its own. Before commit,
push, or merge, obey the active profile in the root `AGENTS.md`:

- **Conservative / minimal / default**: report status and proposed commands;
  wait for explicit user/orchestrator authorization before commit/push.
- **Team-maintainer** (only when the repository explicitly opts in): commit
  and push may run as part of the loop. A current "do not commit" or "do not
  push" instruction still wins.

**Merge always additionally requires the explicit user command "мерж pr".**
This skill must never auto-merge, regardless of profile.

## Pr

1. Загрузи `beads` skill и используй `bd` для задач.
2. `bd update <id> --claim` перед commit. `bd close <id> --reason "..."`
   только после merge в `master`.
3. Заказчик даёт серьёзный code review; P1 обязательны к исправлению,
   P2 на усмотрение.
4. Команды заказчика: «коммит», «коммит и пуш», «коммит пуш», «мерж pr»,
   «бери следующую таску».
5. PR body должен описывать фактический текущий contract/поведение,
   а не устаревший.
6. Не мержить непроверенные PR; мержить только по явной команде.
7. Коммиты с заголовком + body description.
8. После merge синхронизировать локальный `master`
   (`git checkout master && git pull --ff-only`).
9. Push обязателен ДО запроса на ревью — ревьюер смотрит GitHub.
10. work9flow validation gates перед пушем: `go vet ./...`, `go test ./...`,
    `make smoke`. Для PR с реальным DSH: `make smoke-real-dsh`. Эти
    gates обязательны для изменений runtime path; docs-only PR может их
    пропустить (по усмотрению).

## Reviewer chat URL — никогда не в репозитории

Конкретный ChatGPT conversation URL — это per-account / per-session данные.
Никогда не коммить его в `SKILL.md`, `bin/*`, `providers.toml`, ни в какой
tracked-файл.

Резолв URL (по приоритету):

1. `$WORK9FLOW_REVIEW_CHAT_URL` (env, например в `~/.config/work9flow/env.sh`).
2. `chat_url:` в `$WORK9FLOW_REVIEW_CHAT_CONFIG`
   (default `~/.config/work9flow/review-loop.yaml`).

Helper:

```bash
w9-review-chat-url
```

Если helper падает с exit ≠ 0 — skill **не может продолжить**: конфиг
отсутствует или пуст. Не выдумывай URL, не коммить его как fallback.

## Reviewer marker protocol

Каждый запрос на ревью ОБЯЗАН требовать от ревьюера завершать ответ точной
строкой в формате:

```
REVIEW_END <full-commit-sha> APPROVE
```

или

```
REVIEW_END <full-commit-sha> REQUEST_CHANGES
```

где `<full-commit-sha>` — это `git rev-parse HEAD` ветки, на которой живёт
PR (на момент отправки запроса).

Сам submitted request **содержит** эту строку как шаблон, поэтому polling
должен детектить **только verdict-bearing** marker
(`REVIEW_END <sha> APPROVE` или `REVIEW_END <sha> REQUEST_CHANGES`),
а не голый `REVIEW_END <sha>`. Иначе polling сам себе сматчится.

Helper:

```bash
w9-review-wait "$SHA" --timeout 600 --interval 10
# exit 0 → APPROVE, 2 → REQUEST_CHANGES, 3 → timeout
```

Каждый цикл polling — один, максимум 60 секунд. По таймауту — ручной
`snapshot` + ручная проверка.

## Iteration loop

```text
bd ready
bd show <id>
bd update <id> --claim
# design / implement / unit tests
go test ./...
go vet ./...
make smoke            # если меняется runtime path
git add <files>
git commit -m "title" -m "body"
git push -u origin HEAD                       # push current branch as-is
PR_URL="$(gh pr create --base master --head "$(git rev-parse --abbrev-ref HEAD)" \
            --title "<title>" --body-file /tmp/pr-body.md)" \
  || gh pr view --json url -q .url           # idempotent: use existing PR if any
SHA="$(git rev-parse HEAD)"
chat_url="$(w9-review-chat-url)"
cdt select_page 2 >/dev/null 2>&1 \
  || cdt navigate_page --url "$chat_url" >/dev/null
# Safe-shell pattern: write body to file, load into var, pass as single arg.
request_file="$(mktemp)"
cat >"$request_file" <<BODY
PR: $PR_URL
Branch: $(git rev-parse --abbrev-ref HEAD)
Bead: <bead-id> — <bead-title>
SHA: $SHA
...
BODY
uid="$(cdt take_snapshot 2>/dev/null | awk '/textbox/ { match($0,/uid=([0-9_]+)/,a); if(a[1]){print a[1]; exit} }')"
value="$(cat "$request_file")"
cdt fill "$uid" "$value"
cdt press_key Enter
loop w9-review-wait "$SHA" --timeout 60 --interval 5
  exit 0 → APPROVE: ждать "мерж pr"
  exit 2 → REQUEST_CHANGES: адресуй P1 (P2 по усмотрению), коммить, пуш, новый SHA, повтор
  exit 3 → таймаут: cdt take_snapshot вручную, проверить в uids после моего submit, решить
on "мерж pr":
  gh pr merge --squash --delete-branch
  git checkout master && git pull --ff-only
  bd close <id> --reason "..."
```

## Review request template

Пиши в файл, не интерполируй в shell:

```text
PR: <URL>
Branch: <branch>
Bead: <bead-id> — <bead-title>
SHA: <full-commit-sha>

Что в этом PR:
<1-3 строки>

Файлы:
<key files touched>

Прошу code review. P1 должны быть исправлены обязательно, P2 — на твоё
усмотрение. Заверши ответ точной строкой:

REVIEW_END <full-commit-sha> APPROVE

или

REVIEW_END <full-commit-sha> REQUEST_CHANGES

После APPROVE скажи «мерж pr» я смержу. После REQUEST_CHANGES я внесу
правки и снова отправлю на ревью с новым SHA.

Что делать дальше?
```

## Chrome-devtools quick commands

```bash
# Резолв URL (env/config)
w9-review-chat-url

# Список вкладок (найти индекс work9flow review chat)
cdt list_pages

# Переключиться / открыть review chat
cdt select_page <index> || cdt navigate_page --url "$(w9-review-chat-url)"

# Свежий snapshot
cdt take_snapshot

# Безопасная отправка текста (без shell interpolation)
request_file="$(mktemp)"
cat >"$request_file" <<'BODY'
... длинный текст ...
BODY
uid="$(cdt take_snapshot | awk '/textbox/ { match($0,/uid=([0-9_]+)/,a); if(a[1]){print a[1]; exit} }')"
value="$(cat "$request_file")"
cdt fill "$uid" "$value"
cdt press_key Enter

# Polling с verdict detection
w9-review-wait "$SHA" --timeout 60 --interval 5
```

## Helpers (personal automation, optional)

Эти скрипты — **personal automation**. Они НЕ коммитятся в репозиторий и не
обязательны: всё можно делать руками через `cdt`. Если они есть на `$PATH`
(например `~/.local/bin/`), skill использует их как syntactic sugar.

- `w9-review-chat-url` — резолвит ChatGPT conversation URL из
  `$WORK9FLOW_REVIEW_CHAT_URL` или `~/.config/work9flow/review-loop.yaml`.
  Без конфига — exit 2.
- `w9-review-wait <sha>` — polling cdt snapshot, ищет
  **verdict-bearing** marker (только когда marker — это всё содержимое одной
  StaticText-ноды; отличает reviewer reply от моего submitted template).
  Выходит с 0/2/3.

## Anti-patterns

- ❌ Коммитить account/session-specific URL в любой tracked-файл.
- ❌ Polling с `wait_for --timeout > 60000` — блокирует очередь.
- ❌ Ждать без маркера — старые ответы в чате ломают цикл.
- ❌ Считать «голый `REVIEW_END <sha>`» за verdict — он есть в моём
  submitted request.
- ❌ Помечать beads закрытой до реального merge в `master`.
- ❌ Делать merge без явной команды «мерж pr».
- ❌ Писать коммиты без body / description.
- ❌ Копировать machine-specific пути в коммит/PR.
- ❌ Запускать браузерные сессии, пока старый wait_for не истёк —
  каждый цикл опроса ровно один.
- ❌ Игнорировать P1 в комментариях ревьюера.
- ❌ Делать `git push -u origin feat/<scope>` — завязка на naming convention;
  push всегда по `HEAD`.
- ❌ Делать `gh pr create` без проверки существующего PR — должен быть
  idempotent.
- ❌ Строить `cdt fill "<text>"` через небезопасную shell interpolation —
  всегда через `value="$(cat file)"` + `cdt fill "$uid" "$value"`.

## Quick commands

```bash
# Состояние ревью-сессии
git rev-parse HEAD
gh pr view --json number,url,title,headRefOid
gh pr checks "$(gh pr view --json number -q .number)" 2>/dev/null || true

# URL для этой сессии
w9-review-chat-url

# Ручной snapshot (на таймауте polling)
cdt take_snapshot | tail -200
```

## Что НЕ нужно комитить

- Реальный conversation URL (`chatgpt.com/c/<id>`).
- Имя пользователя, email, токены, account-specific настройки.
- `~/.config/work9flow/review-loop.yaml` — должен быть в user-level gitignore.
