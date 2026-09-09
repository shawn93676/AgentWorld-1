// Package village —— Agent Village 的 sdk.Module 实现（AV-01）。
//
// 边界（g1.txt 最重要的技术决策）：
//   - AgentWorld Runtime 负责"世界怎么活"（调度 / Think 循环）；
//   - 本模块只实现 sdk.Module 六件套：Perceive / Planner / Executor / WakePolicy / OnBoot；
//   - Village 的游戏规则（地点、经济、日程、关系）全部在 village 领域包，不侵入 Runtime。
//
// 决策分层：Planner 是纯规则（Level 0，零 LLM 成本）；只有玩家对话在配置了
// LLM_API_KEY 时经 ChatLLM 适配器走高级模型（Level 2）。
package village

import (
	"context"
	"strings"
	"sync"

	"agentworld/internal/llm"
	"agentworld/sdk"
	"agentworld/worlds/village/village"
)

// Module 村庄世界模块。
type Module struct {
	world *village.World
	wake  dueWakePolicy

	mu      sync.Mutex
	pending map[int64]*village.Activity // Planner 决策 → Executor 应用（Think 同协程顺序调用）
}

// New 创建模块（world 由 main 构建并 Attach 完 Agent）。
func New(w *village.World) *Module {
	m := &Module{world: w, pending: map[int64]*village.Activity{}}
	m.wake.world = w
	return m
}

// Game 返回世界（供 server 访问）。
func (m *Module) Game() *village.World { return m.world }

// EnableLLM 注入玩家对话 LLM（nil / 未配置 = 纯模板回复，零成本）。
func (m *Module) EnableLLM(c *llm.Client) {
	if c == nil || !c.Enabled() {
		return
	}
	m.world.SetChatLLM(&chatAdapter{c: c})
}

func (m *Module) Name() string { return "village" }

// Perceive 构建感知。
func (m *Module) Perceive(ctx context.Context, a sdk.Agent) (sdk.Perception, error) {
	return m.world.Perceive(a.ID), nil
}

func (m *Module) Planner() sdk.Planner       { return &planner{m: m} }
func (m *Module) Executor() sdk.Executor     { return &executor{m: m} }
func (m *Module) WakePolicy() sdk.WakePolicy { return &m.wake }

func (m *Module) OnBoot(rt sdk.Runtime) error { return nil }

// ---- Planner（规则决策，Level 0） ----

type planner struct {
	m *Module
}

func (p *planner) Decide(ctx context.Context, a sdk.Agent, perc sdk.Perception) (*sdk.Decision, error) {
	act := p.m.world.DecideFor(a.ID)
	if act == nil {
		return nil, nil
	}
	p.m.mu.Lock()
	p.m.pending[a.ID] = act
	p.m.mu.Unlock()
	dec := &sdk.Decision{
		Action:  act.Kind,
		Content: act.Place,
		Reason:  strings.Join(act.Reason, " | "),
	}
	if act.TalkTo != "" {
		dec.TargetKind = "agent_name"
		dec.Content = act.TalkTo
	}
	return dec, nil
}

// ---- Executor（把决策落地到共享世界） ----

type executor struct {
	m *Module
}

func (e *executor) Execute(ctx context.Context, rt sdk.Runtime, a sdk.Agent, perc sdk.Perception, dec *sdk.Decision) (string, error) {
	e.m.mu.Lock()
	act := e.m.pending[a.ID]
	delete(e.m.pending, a.ID)
	e.m.mu.Unlock()
	if act == nil {
		return "", nil
	}
	return e.m.world.ApplyDecision(a.ID, act), nil
}

// ---- WakePolicy（Due 唤醒：只唤醒需要决策的 Agent） ----

type dueWakePolicy struct {
	world *village.World
}

func (p *dueWakePolicy) Select(ctx context.Context, rt sdk.Runtime, triggered, all []sdk.Agent) []sdk.Agent {
	out := make([]sdk.Agent, 0, len(all))
	for _, a := range all {
		if p.world.IsDue(a.ID) {
			out = append(out, a)
		}
	}
	return out
}

// ---- 玩家对话 LLM 适配器（Level 2：只有玩家对话走高级模型） ----

type chatAdapter struct {
	c *llm.Client
}

func (ad *chatAdapter) Enabled() bool { return ad.c.Enabled() }

func (ad *chatAdapter) Chat(ctx context.Context, system, user string) (string, error) {
	return ad.c.ChatText(ctx, system, user)
}
