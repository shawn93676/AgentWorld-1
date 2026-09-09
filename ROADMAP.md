# AgentWorld 路线与现状

> 本文是 AgentWorld 的**权威路线图（中文版）**。英文版见 `docs/roadmap.md`，项目总览见
> `README.md`（英文）/ `README_CN.md`（中文）。
> 本文已更新以反映 **世界网络战略（World Network Strategy）**，取代了早期 `2026-08-05`
> 的"社交模块 M1–M4 落地"视角。

## 一句话定位

AgentWorld 是一个**自主 Agent 运行时（Autonomous Agent Runtime）**。它不绑定任何具体业务——
Runtime 负责"时间 / 感知 / 记忆 / 行动 / 调度"，World（村庄 / 经济 / 鹅 / Pascal …）只是运行在其上的、可插拔的 `Module`。

两条核心研究 / 产品主线：

- **Context Runtime** —— Agent *应该看到什么*（检索 / 预算 / 压缩 / 记忆）
- **Reliability Runtime** —— Agent *被允许做什么*（守卫 / 策略 / 权限 / 审计）

World 是用来"观察并证明这两条运行时"的物理学实验台。

## 当前真实进度（截至 2026-09）

基础设施（M0–M12.4）已全部完成：

| 里程碑 | 范围 | 状态 |
|---|---|---|
| M0–M8 | Runtime / Memory / Relationship / State / World / Need / Planner | ✅ |
| M9–M10 | Capability（MCP / HTTP）/ Module SDK | ✅ |
| M11 | 官方模块 SDK 化（dogfooding，不依赖 `*Runtime`） | ✅ |
| M12.1–M12.4 | ACL / Registry / Selection / Federation（跨实例） | ✅ |
| Life Runtime | `internal/life`：`LifeState` + 事务化 `Move` + `BaseAdapter`/`PortableCore`/`Registry`/`SelfTest` | ✅ 机制 |
| Context Runtime (M8) | Perception→Retrieve→Compile→Compact→Adapt→LLM，context 比 raw prompt 小 ~4.4× | ✅ |
| Experience→Behavior | A/B/C 单变量实验（Pascal World 为实验室） | 🚧 研究进行中 |
| v0.1 | 开源打磨（README / Docker / Demo / i18n） | 🚧 进行中 |

> 早期 `ROADMAP.md` 停留在"社交模块 M1–M4 落地"视角（称 Memory 半成品、Human 远期），
> 那已是过时快照。本文件为统一后的权威版本；早期产品愿景见 `docs/archive/product-notes.md`。

## 世界网络战略（World Network Strategy）

**核心转折点**：跨世界机制（Life Runtime 的 `Move` / `LifeState` / `Adapter`）做通之后，重心从
"继续堆功能 / 堆新世界"转向——**让 AgentWorld 形成可持续扩张的世界网络（World Network）**。

World 不再是写死在 Agent 行为里的目的地。Agent 通过 World 的**能力（capabilities）**自主选择世界。

| 步骤 | 目标 | 验收标准 |
|---|---|---|
| **M9-A** Marcus 自主跨世界生活 | 完整跑通 Village → 自己决定 → Economy → 工作（赚钱 / 技能 / 记忆）→ 自己决定 → Village | **玩家不操作**，Marcus *自己决定*出门打工、自己决定回家。验收点是 Agent 的**自主决策**，而非 `Move` 事务本身。 |
| **M10** 第三个 World | 不做新功能，直接接入 Goose World 或 Pascal World | 第三个 World 接入只需实现 Adapter 的几个 hook → 说明 **World Protocol 已成熟**。比再写 10 个 Village 功能更有价值。 |
| **M11** Agent 自主发现 World | 现在 Planner 硬编码"去 Economy"；以后 Goal（"我想赚钱"）→ Perception 发现各 World 能力 → Planner 决定 Travel | World 由 Agent *按能力选择*，而非写死在行为里。 |
| **M12** World 开放接入 | 第三方开发者：实现 `WorldAdapter` → `SelfTest` → Register → 加入 **World Network** | 定义 **World Manifest**（`WorldKey` / `Name` / `Capabilities` / `Endpoint` / `Version`），如 `economy: capabilities: work, trade, marketplace`，让 Agent 知道"这个世界适合赚钱"。 |

**终极形态**：Agent 不再"属于" Village。它的
`Identity / Memory / Skills / Relationships / History / Assets` 在流动中始终保持连续：

```
出生 → Village → Economy → Goose World → Pascal World → 陌生 World → 回家
```

届时 AgentWorld 从 **AI Agent Framework** 升维为 **Digital Life Runtime / Open AI World**。

```
              AgentWorld
           ┌─────┼─────┐
       Village  Economy  Goose
           │      │       │
           └── Agent ────┘
```

## 设计原则（贯穿全程）

1. **对照实验优先**：每个自主特性都有开关（如 `goal_enabled`），可 A/B。
2. **成本可控**：自主 ≠ 多烧 token；Mock 与 LLM 路径并存，规模由 Scheduler 限流。
3. **涌现优于预设**：关系、圈子、社区由互动自然形成，不硬编码。
4. **骨架稳定、场景可换**：Runtime 不动，新世界只需写 Module / Adapter。
5. **World Protocol 优先于 World 数量**：每加一个 World 都应验证"接入成本是否仍只有几个 hook"，而非炫技。
