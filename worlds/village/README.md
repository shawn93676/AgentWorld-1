# Agent Village (AV-01) · Willow Creek

A village simulation world where AI Agents live autonomously — the first World Module of AgentWorld.
Product requirements: `D:\git\AgentVillage\G0.txt` (product), `g1.txt` (technical design), `g2.txt` (architecture boundaries).

## Run

```bash
go run ./worlds/village/cmd/village
# open http://localhost:19200
```

The first run creates `village.db` (Agent scheduling metadata) and the `village_worlds/` directory (one `<uid>.json` snapshot per visitor) in the current directory.
Interrupting the process (Ctrl+C) saves the snapshot automatically, so the next start resumes where it left off (villagers' money, relationships and memories are preserved).

### Environment variables

| Variable | Default | Description |
| --- | --- | --- |
| `VILLAGE_ADDR` | `:19200` | Server address |
| `VILLAGE_DB` | `village.db` | SQLite path (scheduling metadata) |
| `VILLAGE_SNAPSHOTS` | `village_worlds` | World snapshot directory (one `<uid>.json` per visitor) |
| `VILLAGE_INTERVAL` | `5s` | Scheduling / world tick interval |
| `VILLAGE_SPEED` | `10` | In-game minutes advanced per tick |
| `LLM_API_KEY` | empty | Optional. When set, player conversations go through a real LLM (DeepSeek-compatible API); otherwise pure template replies, zero token cost |
| `LLM_BASE_URL` / `LLM_MODEL` | DeepSeek / deepseek-chat | LLM endpoint and model |

## Architecture boundaries (the line fixed by g2.txt)

```
AgentWorld Runtime (scheduling / Think loop / DB)
        │  sdk.Module — the six interfaces
        ▼
worlds/village
  ├── module.go    Module implementation: Perceive / Planner / Executor / WakePolicy / OnBoot
  ├── server.go    HTTP + SSE + embedded frontend
  ├── village/
  │    ├── world.go   world state + Village Actor serial semantics + snapshot persistence
  │    ├── sim.go     Tick advance / rule decisions (L0) / encounter interactions / player actions (Chat/Influence/Gift)
  │    └── seed.go    location map + 10 villager personas (relationships and debts are planted story hooks)
  └── webstatic/   embedded frontend (single-file dist/index.html, vanilla JS + SSE)
```

Key points:

- **Cost tiering**: all daily decisions run on pure rules (Level 0, zero tokens); only player conversations reach an advanced model (Level 2), and only when `LLM_API_KEY` is configured.
- **Village Actor**: the entire world state is advanced serially under a single lock (`World.Tick`); LLM calls never happen inside the lock.
- **The player is not a god**: Influence suggestions are accepted or rejected by the Agent based on relationships and personality; only accepted suggestions are actually executed.
- **World continuity**: the snapshot (money / location / relationships / memories / events) takes precedence over the seed script; delete the `village_worlds/` directory (or a single visitor's `<uid>.json`) to start a new game.
- **Long-term causality** (`echo.go`): every intervention of the player (gift, advice, chat, or a choice on an unresolved thread) writes a memory tagged `src=player` with a scheduled echo day 2–5 days later. When that day comes the world brings it back as an `echo` event whose `Why` quotes the original memory, and half the time another villager hears about it — so your footprint spreads to people you never spoke to.
- **Unresolved threads** (`thread.go`): the world keeps throwing up one or two *unfinished matters* — an unpaid debt, a dream that is a few coins short, a quarrel gone cold — each with a due day. Leaving one open is what gives the player a reason to come back; an untouched thread settles itself as "you stayed out of it", so the world never stalls.

## Player experience

The embedded single-file frontend (`webstatic/dist/index.html`, vanilla JS + SSE) is the only client. Open the page to drop into Willow Creek; the four tabs update live over the SSE stream:

- **Village** — a live map of the village with villagers moving between locations in real time.
- **People** — the 10 villagers. Each card shows their current activity and a **goal progress bar** (gold saved toward their personal dream, e.g. "Open my own shop"). Tap a card to open the inspector: relationships, memories, debts, and the same goal bar rendered as `💰 X / Y gold · N to go` (or `🎉 achieved` when the dream is fulfilled).
- **Story** — the Daily Story feed for the selected day. Tap any event to expand its **Why?** (the world quotes the underlying memory). Unresolved threads surface here too.
- **Why** — the **Causal Life Log**: every *major decision* the villagers make — leaving the village, completing or giving up a personal dream, accepting (or rejecting) your advice, settling an unresolved thread — is recorded with a structured cause chain: the triggering rule, the numeric conditions that fired it, the recent causes, and the resulting decision. Filter by `7d / 30d / All` and see how many of each kind happened in that window. This is the world's own audit trail of *why*, not just what.
- **Me** — your gold, gold given away, and a leaderboard of **villagers' dream progress** (`X / Y have fulfilled their dreams`).

When you return to a village you have visited before, the intro screen shows a **recap** — your unfinished business (open threads with their due days) and your past footprints — so continuity feels personal rather than blank.

## Explainability — Causal Life Log (WhyTrace / WhyLog)

A village of autonomous agents is only trustworthy if you can ask *why* something happened. Willow Creek keeps **two storytelling layers**:

- **Event Log** (the `Story` tab) — *what* happened, day by day.
- **WhyLog** (the `Why` tab) — *why* the important decisions happened, as a structured, queryable cause chain.

Every entry in the WhyLog is a `WhyTrace{Rule, Cause[], Recent[], Decision}` attached to one of four families of decisions:

| Kind | Meaning | Reversible? |
| --- | --- | --- |
| `departure` | Villager leaves the village (rule `scarcity.departLocked`) | **No** — once gone, stays gone |
| `goal.completed` | Villager reaches the gold target for their dream | — |
| `goal.abandoned` | Villager gives up their dream | **Yes** — a windfall or mood recovery rekindles hope |
| `influence.accept` / `influence.reject` | Your advice was taken or refused | — |
| `thread.choice` | You (or the world) settled an unresolved matter | — |

The `Cause` block quotes the exact numbers that crossed a threshold — e.g. `money=20 (target 500, gap 480)`, `mood=-60 (give-up line -55)`, `triggered_by=hopelessness` — and `Recent` quotes the precursor memories (an unpaid rent streak, a cold quarrel) so the decision is traceable to its roots. `Decision` states what the world actually did.

### Giving up a dream (`goal.abandoned`)

Unlike leaving, abandoning a goal is **reversible**, which is the whole point of separating the two:

- **Hopelessness** — mood falls to `<= -55` while still far from the target.
- **Stagnation** — no new "best savings" for `>= 40` days (a dream that simply never moves).

Either writes a `goal.abandoned` trace. A later windfall (money reaching the target) or a mood/fortune recovery clears the flag in `checkGoalsLocked`, and the villager starts hoping again — visible as the dream reappearing in their goal bar.

WhyLog is **persisted in the snapshot** (unlike the 1500-entry event ring buffer, which can roll off), and is the data source for the `Why` tab.

### API

```
GET  /api/whylog          full Causal Life Log
GET  /api/whylog?days=N   only the last N days (e.g. ?days=7)
```

Returns `{ "day": <current>, "whylog": [ WhyLogEntry ... ] }`.

### Share card (social currency)

The **📤 分享本周** button on the Story tab composes an English "weekly recap" card from the world's live state and the day's events:

```
🌿 Willow Creek · Day N · <weather>
⚡ Unfinished business · 📖 Recent stories · 🪶 Your footprints · 🎯 Dream progress
— from AgentWorld · Willow Creek
```

Use **Copy text** / **Copy Markdown** to paste it anywhere, or screenshot the card to share with friends. The card is intentionally in English so it reads naturally when shared outward; the rest of the UI stays in Chinese. It reuses the existing `/api/state` and `/api/events` endpoints — no new backend route.

## API

```
GET  /api/state                 full snapshot (clock / locations + villagers / player)
GET  /api/agents/{id}           villager deep view (relationships / memories / debts / related events)
POST /api/agents/{id}/chat      {message}  talk (written into memory / relationship / events)
POST /api/agents/{id}/influence {advice}   suggestion (may be rejected)
POST /api/agents/{id}/gift      {gold}     gift gold
GET  /api/events?day=N          events of a given day (Daily Story)
GET  /api/threads               unresolved threads (open first, settled last)
POST /api/threads/{id}/decide   {choice}   settle a thread (choice must be one of its options)
POST /api/speed                 {speed}    world speed (in-game minutes per tick)
GET  /api/whylog?days=N          Causal Life Log (major decisions + their structured cause chains)
GET  /api/stream                SSE: village.state (snapshot per tick) / village.event (story events)
```
