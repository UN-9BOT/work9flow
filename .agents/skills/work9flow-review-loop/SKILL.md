---
name: work9flow-review-loop
description: Coordinate a work9flow contributor loop that takes work from `bd`, pushes branches to GitHub, opens a PR, and runs reviewer-ratified code review through ChatGPT via the chrome-devtools-cli skill. Local-only knowledge; not for the public repo.
allowed-tools: Bash(bash *)
---

# work9flow review loop

bead → work → commit → push → PR → chrome-devtools review request →
fix P1 → re-request → approve → merge → next bead.

## Pr

1. Перед началом работы загрузи `beads` skill и используй `bd` для задач.
2. Перед commit: `bd update <id> --claim`. После merge в `master`: `bd close <id> --reason "..."`.
3. Заказчик даёт серьёзный code review; P1 обязательны к исправлению, P2 на усмотрение.
4. Команды заказчика: «коммит», «коммит и пуш», «коммит пуш», «мерж pr», «бери следующую таску».
5. Перед merge PR body должен описывать фактический текущий contract/поведение, не устаревший.
6. Не мержить непроверенные PR; мержить только по явной команде.
7. Коммиты писать с заголовком + body description.
8. После merge синхронизировать локальный `master` (`git checkout master && git pull --ff-only`).
9. Ревьюер смотрит GitHub — push обязателен ДО запроса на ревью.
10. Используй `chrome-devtools-cli` skill (cdt wrapper) против вкладки
    `https://chatgpt.com/c/6a8d8149-053c-83ed-8ebb-19a81677f837` (chat «Сравнение DeepSeek Harness Codex Claude»).
11. work9flow validation gates перед пушем: `go vet ./...`, `go test ./...`,
    `make smoke`. Для PR с реальным DSH: `make smoke-real-dsh` (gate не
    обязателен для docs-only PR; ревьюер проверит).

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

Триггер ожидания ответа — точное появление префикса
`REVIEW_END <full-commit-sha>` в последнем сообщении ревьюера (APPROVE или
REQUEST_CHANGES).

Политика опроса:

- `cdt wait_for --text "REVIEW_END <sha>" --timeout 60000` (1 минута).
- По таймауту снимать свежий `cdt snapshot`, проверять наличие маркера
  вручную и повторять цикл.
- Не полагаться на старые `APPROVE`/`REQUEST_CHANGES` из истории чата:
  ревью-сессия идентифицируется ТОЛЬКО по `REVIEW_END <sha>` SHA.

Проверка фактического head перед запросом:

```
git rev-parse HEAD
gh pr view --json number,url -q .number       # sanity: PR already open
gh pr checks $(...)                            # CI status if any
git status --short --branch                   # clean tree
```

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
git push -u origin feat/<scope>
gh pr create --base master --title "<title>" --body "..."
SHA=$(git rev-parse HEAD)
cdt navigate https://chatgpt.com/c/6a8d8149-053c-83ed-8ebb-19a81677f837
cdt fill "<REVIEW_REQUEST_TEMPLATE>"           # see template below
cdt press_key Enter
loop cdt wait_for "REVIEW_END $SHA" --timeout 60000
  on snapshot still typing or no marker → снимать snapshot, проверять
address each P1 (P2 — по усмотрению)
commit fix + push
re-request review with new SHA
on "REVIEW_END $SHA APPROVE" — wait for explicit "мерж pr"
gh pr merge --squash --delete-branch
git checkout master && git pull --ff-only
bd close <id> --reason "..."
```

## Review request template

When sending for review:

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
# List tabs (find the work9flow review chat)
cdt list_pages

# Switch to or open the review chat
cdt navigate https://chatgpt.com/c/6a8d8149-053c-83ed-8ebb-19a81677f837

# Snapshot current page (read last assistant message)
cdt snapshot

# Fill the prompt textarea and submit
cdt fill '<text>'
cdt press_key Enter

# Wait up to 60s for a marker in any new message
cdt wait_for --text "REVIEW_END $SHA" --timeout 60000
```

## Anti-patterns

- ❌ Отправлять ревью на локальный head без push (ревьюер смотрит GitHub).
- ❌ Ждать «как закончит» без маркера — старые ответы в истории чата
  ломают цикл.
- ❌ Использовать `wait_for` с `timeout` > 60000 и блокировать очередь.
- ❌ Делать merge без явной команды «мерж pr».
- ❌ Помечать задачу (beads) закрытой до реального merge в `master`.
- ❌ Писать коммиты без body / description.
- ❌ Копировать machine-specific пути в коммит/PR.
- ❌ Запускать браузерные сессии пока старый wait_for не истёк — каждый
  цикл опроса ровно один.
- ❌ Игнорировать P1 в комментариях ревьюера.

## Quick commands

```bash
# Снять текущее состояние ревью-сессии
git rev-parse HEAD
gh pr view --json number,url,title
gh pr checks $(gh pr view --json number -q .number) || true

# Подождать 1 минуту и снять snapshot самому
sleep 60 && cdt snapshot | tail -200
```
