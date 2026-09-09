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
- **Me** — your gold, gold given away, and a leaderboard of **villagers' dream progress** (`X / Y have fulfilled their dreams`).

When you return to a village you have visited before, the intro screen shows a **recap** — your unfinished business (open threads with their due days) and your past footprints — so continuity feels personal rather than blank.

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
GET  /api/stream                SSE: village.state (snapshot per tick) / village.event (story events)
```
