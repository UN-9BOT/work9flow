---
type: agent-handoff
scope: work9flow / agent durable memory
purpose: compact-resilient handoff for work9flow contributor loop
updated: 2026-08-26
---

# work9flow contributor goal — durable handoff

## Глобальная цель (из README.md)
**"Local-first workflow orchestrator. Go TUI + runtime, built on top of
DeepSeek Harness."**

Полный pipeline end-to-end: scout → planner → gatekeeper → implementer → reviewer,
через реальный upstream DSH (deepseek-ai/dsh), без prod-mocks, без fabrication.

## Как я работаю
Skill: `work9flow-review-loop` (`.agents/skills/work9flow-review-loop/SKILL.md`).

Цикл одной bead:
1. `bd ready` → выбрать bead
2. `bd update <id> --claim` → взять в работу
3. **TDD**: red → green (сначала failing test, потом минимальная реализация)
4. `go vet ./...` + `go test ./... -count=1` + (если TS) `npm run typecheck && npm test`
5. `git commit` с body
6. `git push` в origin (по HEAD, не `-u origin <branch>`)
7. Открыть PR (или обновить существующий)
8. Отправить на reviewer через chrome-devtools-cli (page 2: "Сравнение DeepSeek Harness Codex Claude")
9. Ждать `REVIEW_END <sha> APPROVE|REQUEST_CHANGES`
10. REQUEST_CHANGES → фикс → goto 5
11. APPROVE → ждать команду юзера **«мерж pr»**
12. После merge: `git checkout master && git pull --ff-only`
13. `bd close <id> --reason "..."` (только после merge в master!)
14. goto 1

**Решения о следующей bead** принимаются через reviewer verdict (не через юзера).

## Авторитет юзера (минимизирован)
- **Можно без юзера**: claim bead, TDD, commit, push, открыть PR, отправить reviewer,
  фиксить по REQUEST_CHANGES, закрыть bead после merge.
- **Нужна явная команда юзера**:
  - **«мерж pr»** — merge в master (никогда не auto-merge)
  - Смена направления / приоритетов / destroy work
  - `git push -u origin feat/<scope>` (skill запрещает — push всегда по HEAD)
  - `gh pr create` для веток вне `feat/*` convention

## Текущее состояние (на 2026-08-26, branch `feat/dsh-bridge`)
- HEAD: `377003b dsh-A.10e: cut ProvidersFile/localdsh from production code (test-only)`
- PR #15 OPEN: https://github.com/UN-9BOT/work9flow/pull/15
- mergeable: MERGEABLE
- Reviewer chat: page 2 (chrome-devtools)

## Готово к merge (reviewer not yet APPROVED)
**work9flow-92b · dsh-A.10f: server-identity check + COMPATIBILITY.md** · P1
- 3 коммита: ff4517e (initial, RC), bd977b1 (rework), b27f5ad (polish)
- Reviewer agreed концептуально правильный на bd977b1 + polish на b27f5ad

**work9flow-8w0 · dsh-A.10e: cut ProvidersFile/localdsh from production** · P1
- 1 коммит: 377003b
- Reviewer request отправлен, ждём verdict

## Готово к работе (bd ready)
- work9flow-azy · P1 · dsh-A.10g: pin .npmrc to npmmirror.com
- work9flow-7dh · P1 · dsh-A.10h: real assembled DSH smoke (MINIMAX_APIKEY)
- work9flow-zb4 · P1 · dsh-B: activity lifecycle + upstream SessionEvent vocabulary
- work9flow-4v1.10.3 · P1 · dsh-A.10c: superseded by 7dh

## Известные юзер-decisions (2026-08-25)
1. ✅ **localdsh из production** — done в 377003b
2. ✅ **MINIMAX_API_KEY совместим с DSH** — confirmed через web search (DSH supports any OpenAI-compatible endpoint via cordis). Нужен собранный dsh-jsonrpc-agent (отдельная задача 7dh).
3. ✅ **Compatibility check policy** — fail-hard, без patch-level leniency, без warning-only — done в bd977b1 + b27f5ad
4. ⏳ **Registry mirror** — закрепить `registry.npmmirror.com` в `runtime/dsh-bridge/.npmrc` — bead azy
5. ⏳ **b8c + 1jo (activity lifecycle + upstream vocabulary)** — отдельный epic 4v1.11 — bead zb4

## Open P1 от reviewer (для APPROVE PR #15)
- b8c activity interval
- 1jo real SessionEvent.type vocabulary
- concurrent route pin race
- ✅ production localdsh removal (DONE 377003b)
- canonical npm lock (bead azy)
- typed transport/protocol errors
- assembled real-DSH gate (bead 7dh)

## Anti-patterns (skill forbidden)
- ❌ auto-merge без «мерж pr»
- ❌ verbatim `REVIEW_END <sha> APPROVE` в user prompt ревьюеру (self-trigger)
- ❌ polling `wait_for --timeout > 60000`
- ❌ коммитить `chat_url`, токены, account-specific настройки
- ❌ `git push -u origin feat/<scope>` — push всегда по HEAD
- ❌ писать production-код под тесты (TDD-правило из AGENTS.md coding_workflow)
- ❌ claim bead после merge в master (`bd close` — после merge, не раньше)
- ❌ удалять поле/функцию без TDD-red-теста

## Validation gates (перед каждым push)
- `cd runtime/dsh-bridge && npm run typecheck && npm test` — для TS изменений
- `go vet ./...` — для Go изменений
- `go test ./... -count=1` — для Go изменений
- `make smoke` — для runtime path изменений (если применимо)

## Reviewer session state
- Page 2 active
- Textbox uid: `1_753`
- Polling: каждые 30с, ищу contiguous `REVIEW_END <sha> VERDICT`
