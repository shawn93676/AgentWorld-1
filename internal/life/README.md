# Life Runtime（`internal/life`）— 世界开发者接入指南

> **窄层，不是第二套框架。** 本包只补 `internal/agent` + `sdk` + `scheduler` 尚未覆盖的
> 两处 M9 概念（`LifeState` 生命周期、`Move` 跨世界事务），不重复实现调度 / 模块 / 持久化。
> 完整说明与 M9 demo 见根目录 [`README.md`](../../README.md) 的 “Life Runtime” 章节。

本指南回答一个问题：**我要给一个新世界接上“可跨世界携带”的能力，最少要写什么？**

## 一句话答案

让世界 adapter **嵌入 `*life.BaseAdapter`**，再实现 **`life.PortableCore`** 的 7 个钩子。
`Leave / Enter / Export / LocalID / SelfTest / Move` 全部免费获得。接第 3、4 个世界 = 复制
一份翻译，**不是复制一份样板**。

## 1. 本包提供什么（你不用写）

| 能力 | 哪来的 | 说明 |
|---|---|---|
| `LifeState` 枚举 + `IsActive()` | 本包 | `alive/sleeping/traveling/dead/archived`；`Think` 循环用它跳过休眠/死亡/旅行者 |
| id 映射（`名字↔本地int64`）+ `revMap` | `BaseAdapter` | 跨世界稳定身份，自动维护 |
| `LifeState` 代管 | `BaseAdapter` | 你不必给世界 Agent 加状态字段也能携带/还原 |
| `Leave` / `Enter` / `Export` / `LocalID` | `BaseAdapter` | 事务进出 + 快照，含锁 |
| `Move(from, to, id)` 事务 | 本包顶层函数 | `pending→Leave→Enter→completed`，失败回滚并标 `failed` |
| `Registry` | 本包 | 按 `WorldKey` 自注册与 `MoveByKey` 寻址 |
| `SelfTest(p)` 往返校验 | `BaseAdapter` | `导出→导入→再导出` 严格相等；每个新世界白送 |

## 2. 你要写的：7 个 `PortableCore` 钩子

```go
type PortableCore interface {
    WorldKey() string                              // 稳定的世界名，如 "village"
    CanAccept(p AgentPortable) bool                // 目标世界能否接纳（如要求至少 1 项技能）
    SeedNames() map[string]int64                   // 初始 名字->本地ID（本世界已有 Agent）
    StoreGet(local int64) (any, bool)              // 查
    StorePut(local int64, ag any)                  // 增改
    StoreRemove(local int64)                       // 删
    StoreAllocLocal() int64                        // 给新入境 Agent 分配未用的本地 ID
    ExportLocked(local int64, ag any) (AgentPortable, error)   // 世界Agent -> AgentPortable
    ImportLocked(local int64, p AgentPortable) (any, error)    // AgentPortable -> 世界Agent
}
```

> **锁约定**：这些钩子被 `BaseAdapter` 在持锁（`b.mu`）状态下调用，可直接用
> `b.NameOf(local)` / `b.StateOf(local)` 读映射与状态；你自己的存储锁（如 `w.mu`）由钩子
> 内部自行加解锁，保持“`b.mu` 先于 `w.mu` 获取”即可避免死锁。

## 3. 最小可运行模板

直接复制 [`life_test.go`](life_test.go) 里的 `mockCore` 改成你自己的 `Agent` 类型即可：

```go
type FooAdapter struct {
    *life.BaseAdapter
    w     *foo.World
    seq   int64          // 旅行者本地 ID 区间起点，避开种子 1..N
}

func NewFooAdapter(w *foo.World) *FooAdapter {
    a := &FooAdapter{w: w, seq: 100000}
    a.BaseAdapter = &life.BaseAdapter{}
    a.BaseAdapter.Init(a)   // 快照 + id 映射 + 生命周期——全部免费
    return a
}

// 然后实现 7 个钩子。下面是只翻译 Skills 的极简版：
func (a *FooAdapter) WorldKey() string { return "foo" }
func (a *FooAdapter) CanAccept(p life.AgentPortable) bool { return p.Identity.Name != "" }

func (a *FooAdapter) SeedNames() map[string]int64 {
    mm := map[string]int64{}
    for id, ag := range a.w.Agents { mm[ag.Name] = id }   // 按你的世界 Agent 结构读取
    return mm
}
func (a *FooAdapter) StoreGet(local int64) (any, bool)    { return a.w.GetAgent(local) }
func (a *FooAdapter) StorePut(local int64, ag any)        { a.w.PutAgent(local, ag.(*foo.Agent)) }
func (a *FooAdapter) StoreRemove(local int64)             { a.w.RemoveAgent(local) }
func (a *FooAdapter) StoreAllocLocal() int64              { a.seq++; return a.seq }

func (a *FooAdapter) ExportLocked(local int64, ag any) (life.AgentPortable, error) {
    x := ag.(*foo.Agent)
    p := life.AgentPortable{
        AgentID:  x.Name,
        Identity: life.PortableIdentity{ID: x.Name, Name: x.Name},
        Life:     life.PortableLife{State: a.BaseAdapter.StateOf(local)},
    }
    for k, v := range x.Skills { p.Skills = append(p.Skills, life.PortableSkill{Name: k, Level: v}) }
    return p, nil
}
func (a *FooAdapter) ImportLocked(local int64, p life.AgentPortable) (any, error) {
    x := &foo.Agent{Name: p.Identity.Name, Skills: map[string]int{}}
    for _, s := range p.Skills { x.Skills[s.Name] = s.Level }
    return x, nil
}
```

接好之后，跨世界搬运一行就够：

```go
t, err := life.Move(villageAdapter, economyAdapter, "Marcus")
```

## 4. 多世界自注册（可选但推荐）

不用在 harness 里手动传 adapter 实例，让它们按 `WorldKey` 互相找到：

```go
reg := life.NewRegistry()
reg.Register(villageAdapter)    // adapter 自带 WorldKey
reg.Register(economyAdapter)
t, err := reg.MoveByKey("village", "economy", "Marcus")   // 按名寻址
```

启动期想校验“所有世界都接对了”，跑一次免费往返：

```go
if errs := reg.SelfTestAll(samplePortable); len(errs) > 0 {
    log.Fatal(errs)   // 任一世界翻译不幂等都会在这里炸出来
}
```

## 5. 让 `LifeState` 真正在运转（不止是个 enum）

世界自己的 `Think` 循环应在决策前守卫：

```go
for _, a := range w.Agents {
    if a.Life.IsActive() == false { continue }   // 跳过 sleeping/dead/traveling
    // ……正常的 Perceive → Planner → Executor……
}
```

参考实现：Village 的 `DecideFor` 与 `Tick` 推进循环都已按此守卫；`life` 包层不强制，
但**只有世界自己守卫，`LifeState` 才会成为模拟的真实输入**。

## 6. 参考实现

- 模板：`life_test.go` 的 `mockCore`（最小可复制）
- Village：`worlds/village/village/life_adapter.go`
- Economy：`worlds/economy/economy/life_adapter.go`（含 `carry*` 侧边 map 暂存记忆/关系）
- demo：`experiments/m9` —— Marcus 走 `village → economy → village`，身份/技能/记忆连续
