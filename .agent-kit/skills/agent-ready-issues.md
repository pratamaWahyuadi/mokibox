---
name: agent-ready-issues
description: "Use when writing issues a dev or AI must execute unaided."
---

# Agent-Ready Issues

An issue meant to be executed by someone with **zero context from your session**
is a different artifact from an issue meant for a teammate who was in the
conversation. The bar is not "clearly describes the feature" — it is **the
implementer never has to ask a question to start, and never has to guess to
finish.**

Trigger: user asks for issues/tickets/backlog a *different* person or agent
will pick up, and says something like "supaya programmer atau AI tahu apa
yang harus dikerjakan tanpa banyak tanya".

## The bar: no questions to start, no guesses to finish

Ask of every line: *could the implementer get this wrong while obeying me
exactly?* If yes, the issue is missing a rule. Three things produce wrong work:

- an unstated convention (a house error-handling pattern, a required index,
  a file that must be edited alongside the obvious one)
- an unstated negative constraint (what must NOT change, which sibling
  behaviour must not regress)
- a named-but-unexplained decision (a weight, a default, a threshold)

The first two are the expensive ones: the work looks finished and is wrong.

## Body structure that works

Ordered so the reader can stop at any level and still act:

1. **Context** — what exists today, in one paragraph, and the concrete gap.
   Cite the file, table, or endpoint that proves the gap. "No search endpoint
   exists" beats "search is missing".
2. **The shape** — signature, wire format, SQL sketch, schema DDL. Concrete
   enough to write the first line of code from. Use fenced blocks for anything
   with exact syntax.
3. **Fork points, with a recommendation** — every place there is a genuine
   design fork, name 2-3 options, say which you recommend and why, and say
   explicitly *"tulis keputusanmu di PR body"*. Do not silently pick one and
   present it as settled; do not dump options without a recommendation either.
4. **Files touched** — new vs modified, and the *non-obvious* ones
   (generated-config files, a sibling test file that will fail to compile).
5. **Conventions that bind this work** — only the ones a newcomer would get
   wrong, each with a pointer to where the full rule lives.
6. **Acceptance criteria** — a checklist a reviewer can run. Every criterion
   must be falsifiable: a status code, a row count, a query output. "works
   correctly" is not a criterion.
7. **Definition of done** — the checks that are not the feature: tests, smoke,
   regression of untouched neighbours, doc/handoff update.
8. **Out of scope** — the things a well-meaning implementer would add.

## Anti-over-engineering (stated in the issue, not just obeyed)

The user's phrase is usually *"pakai best practice tapi jangan over
engineering"*. That is two instructions, and the issue must serve both:

- State the simplest sufficient mechanism as the recommendation
  (offset paging over rank-window paging; hardcoded weights over config that
  nothing will ever change; in-Go dedupe over a staging table).
- Name the specific over-build you are rejecting, so the implementer does not
  "helpfully" add it: no new dependency unless it beats existing shared
  helpers, no new service, no speculative abstraction layer.
- Where a fork exists, say what would have to become true for the heavier
  option to become right. That turns a judgement call into a trigger.

## Dependency edges, and the audit-list trick

When one issue must land before another, say so in **both** directions of
dependency. More importantly: an issue that *filters or extends* something
touched by many other issues (block filters, a new field on a shared wire
object) must carry an **explicit audit list** — every endpoint, query, or file
that must be updated — and require the implementer to report per-item status.
A feature applied to 4 of 6 call sites looks done and leaks.

For a new field on a shared response object: require the implementer to list
which endpoints populate it. Silent partial population is the single most
common way this work ships broken.

## Backlog presentation

When proposing a backlog, order it by **dependency**, not by value. Present:

- grouped by priority tier, each row: title, area, complexity, dependency
- one line per issue on *why that order*
- a short "already exists" inventory so the reviewer can see the basis
- flag issues **blocked by an unmet prerequisite**, and say which

Keep complexity coarse (S/M/L/XL) and honest. If an issue is XL, say what
would split it.

Also: a backlog audit frequently turns up a deficiency that **already exists**
in shipped code (an unbounded counter with no rate limit, a metric that
cannot be trusted). When that happens, do not treat it as a nice-to-have
follow-up — fold it into the issue whose feature depends on that data being
sound, and say in the issue why it is a prerequisite rather than a nicety.

## Writing long issue bodies

Write each body to its own file, then publish. This gives you the ability to
grep/review every body in one pass before any of them is public, and to fix a
systematic mistake in one edit instead of ten.

After writing, verify the output character-by-character rather than by
re-reading: stray non-Latin script and words that lost a space are common in
long single-write documents, and both are invisible on a skim. See the
self-check section of `mokibox-go-shared` for the grep.

## Labels

Prefer existing repo labels. If the user wants a new priority axis, either
create ONE label (`P0`) and put the priority in the title prefix (`P0-3: ...`),
or create the full orthogonal set (`priority:p0..p3`, `complexity:s/m/l/xl`).
Do not create a half-set of a taxonomy. Once issues are up, comment each with
its link to the roadmap doc and the dependency note — a reviewer scanning the
issue list should not have to open every issue to learn the order.
