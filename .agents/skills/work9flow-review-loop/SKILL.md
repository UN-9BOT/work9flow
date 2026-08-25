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

**Marker self-trigger prevention:** user-prompt НЕ должен содержать
verbatim `REVIEW_END <sha> APPROVE` или `REVIEW_END <sha> REQUEST_CHANGES`.
SHA в prompt указывается отдельно; ревьюер строит маркер сам, конкатенируя
три части (`REVIEW_END` + пробел + SHA + пробел + verdict). Только в
reviewer reply marker может появиться verbatim, поэтому polling безопасно
ищет `REVIEW_END <sha> VERDICT` как contiguous substring в snapshot.

Каждый цикл polling — один, максимум 60 секунд. По таймауту — ручной
`snapshot` + ручная проверка в uids после моего submit.

Если у тебя на `$PATH` есть optional helper `w9-review-wait` (см. Helpers
section), он делает ровно то же самое; если нет — пользуйся
`poll_loop`-функцией выше или просто grep.

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

# Resolve chat URL — direct env/config read, no helper required.
if [[ -n "${WORK9FLOW_REVIEW_CHAT_URL:-}" ]]; then
  chat_url="$WORK9FLOW_REVIEW_CHAT_URL"
else
  conf="${WORK9FLOW_REVIEW_CHAT_CONFIG:-$HOME/.config/work9flow/review-loop.yaml}"
  chat_url="$(awk -F': *' '/^[Cc]hat_url:/{v=$2; sub(/[[:space:]]*#.*$/,"",v); if(v!=""){print v;exit}}' "$conf" 2>/dev/null || true)"
fi
[[ -n "$chat_url" ]] || { echo "ERROR: WORK9FLOW_REVIEW_CHAT_URL not set and no chat_url in \$conf" >&2; exit 2; }

# Focus the review chat tab.
cdt select_page "${WORK9FLOW_REVIEW_CHAT_PAGE_INDEX:-2}" >/dev/null 2>&1 \
  || cdt navigate_page --url "$chat_url" >/dev/null
sleep 2

# Safe-shell pattern: write body to file, load into var, pass as single arg.
# Template must NOT contain verbatim 'REVIEW_END <sha> APPROVE|REQUEST_CHANGES'
# — see Review request template section.
request_file="$(mktemp)"
cat >"$request_file" <<BODY
PR: $PR_URL
Branch: $(git rev-parse --abbrev-ref HEAD)
Bead: <bead-id> — <bead-title>
SHA: $SHA
... (template body — see below) ...
BODY
uid="$(cdt take_snapshot 2>/dev/null | awk '/textbox/ { match($0,/uid=([0-9_]+)/,a); if(a[1]){print a[1]; exit} }')"
value="$(cat "$request_file")"
cdt fill "$uid" "$value"
cdt press_key Enter

# Polling: pure cdt loop, no helper required. Look for the verdict-bearing
# marker ONLY. The user prompt does NOT contain 'REVIEW_END $SHA VERDICT'
# verbatim, so the marker can only appear in the reviewer's reply.
#
# Pattern: 'REVIEW_END <sha> APPROVE' or 'REVIEW_END <sha> REQUEST_CHANGES'
# as a contiguous substring anywhere in the snapshot. If on history-paranoia,
# restrict to uids > baseline_uid captured just after submit.
poll_loop() {
  local sha="$1" timeout_s="${2:-60}" interval_s="${3:-5}"
  local deadline=$(($(date +%s) + timeout_s))
  while (( $(date +%s) < deadline )); do
    local snap; snap="$(cdt take_snapshot 2>/dev/null || true)"
    if printf '%s' "$snap" | grep -Eq "REVIEW_END[[:space:]]+${sha}[[:space:]]+REQUEST_CHANGES"; then
      echo "REQUEST_CHANGES" >&2; return 2
    fi
    if printf '%s' "$snap" | grep -Eq "REVIEW_END[[:space:]]+${sha}[[:space:]]+APPROVE([^[:alnum:]]|$)"; then
      echo "APPROVE" >&2; return 0
    fi
    sleep "$interval_s"
  done
  echo "TIMEOUT" >&2; return 3
}
poll_loop "$SHA" 60 5
  rc=$?
  case "$rc" in
    0) ;;  # APPROVE — fall through to pre-merge gate below
    2) echo "REQUEST_CHANGES — адресуй P1, коммить, пуш, новый SHA, повтор"; exit 2 ;;
    3) echo "TIMEOUT — cdt take_snapshot вручную, проверить в uids после моего submit, решить"; exit 3 ;;
  esac

on "мерж pr":
  # PRE-MERGE GATE: the reviewed SHA must still be HEAD locally and on PR.
  # Любое несовпадение → APPROVE invalidated → push → новый review.
  APPROVED_SHA="<sha from latest APPROVE>"
  LOCAL_SHA="$(git rev-parse HEAD)"
  PR_SHA="$(gh pr view --json headRefOid -q .headRefOid)"
  test "$APPROVED_SHA" = "$LOCAL_SHA"   || { echo "APPROVE invalidated: local HEAD moved"; exit 1; }
  test "$APPROVED_SHA" = "$PR_SHA"      || { echo "APPROVE invalidated: PR HEAD moved"; exit 1; }
  test -z "$(git status --porcelain)"    || { echo "APPROVE invalidated: working tree dirty"; exit 1; }
  gh pr view --json state -q .state | grep -q OPEN || { echo "PR not open"; exit 1; }
  gh pr merge --squash --delete-branch
  git checkout master && git pull --ff-only
  bd close <id> --reason "..."
```

## Review request template

Пиши в файл, не интерполируй в shell. **Главное:** user-prompt НЕ должен
содержать verbatim `REVIEW_END <sha> APPROVE` или `REVIEW_END <sha>
REQUEST_CHANGES` — иначе polling сам себе сматчится. SHA указывай отдельно;
маркер ревьюер строит сам, конкатенируя три части.

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
усмотрение.

Заверши свой ответ ОДНОЙ СТРОКОЙ в самом конце, без переносов:

  REVIEW_END <space> <SHA из поля выше, скопированный дословно> <space> <APPROVE|REQUEST_CHANGES>

Никакого другого текста после verdict-строки.

После APPROVE скажи «мерж pr» я смержу. После REQUEST_CHANGES внесу
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

# Polling с verdict detection (без helper — inline функция в loop)
# или, если w9-review-wait на $PATH:
w9-review-wait "$SHA" --timeout 60 --interval 5
```

## Helpers (purely optional, не требуются основным loop)

Эти скрипты — **pure syntactic sugar**, не часть contract. Iteration loop
выше работает БЕЗ них напрямую через `cdt` + встроенную `poll_loop`.

Если хочешь — держи их в `~/.local/bin/` или подобном user-level месте.
Они НЕ коммитятся в репозиторий: их поведение уже воспроизведено в loop
через `cdt`. Если они есть на `$PATH`, можно использовать как shorthand.

- `w9-review-chat-url` — резолвит ChatGPT conversation URL из
  `$WORK9FLOW_REVIEW_CHAT_URL` или `~/.config/work9flow/review-loop.yaml`.
  Без конфига — exit 2. Эквивалент в loop: прямая `awk`-проверка env/config.
- `w9-review-wait <sha>` — polling cdt snapshot, ищет contiguous
  `REVIEW_END <sha> VERDICT`. Выходит с 0/2/3. Эквивалент в loop:
  `poll_loop`-функция выше.

## Anti-patterns

- ❌ Коммитить account/session-specific URL в любой tracked-файл.
- ❌ Polling с `wait_for --timeout > 60000` — блокирует очередь.
- ❌ Ждать без маркера — старые ответы в чате ломают цикл.
- ❌ Считать «голый `REVIEW_END <sha>`» за verdict — он есть в моём
  submitted request.
- ❌ Класть verbatim `REVIEW_END <sha> APPROVE` / `... REQUEST_CHANGES` в
  user prompt — polling сам себе сматчится. Промпт даёт SHA + инструкцию,
  ревьюер строит marker сам.
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
