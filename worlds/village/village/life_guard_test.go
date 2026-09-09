package village

import (
	"testing"

	"agentworld/internal/life"
	"agentworld/worlds/goosegame/goose"
)

// TestLifeStateGuard 验证 Think 循环（DecideFor）对 LifeState 的守卫：
// 在世(alive) 的 Agent 正常决策；休眠/死亡等非活跃状态被直接拦截返回 nil。
func TestLifeStateGuard(t *testing.T) {
	obs := goose.NewObservatory(goose.ObservOpts{})
	w := NewWorld(obs, "")
	w.Attach(1, Profile{Name: "Marcus", Workplace: "market"})

	// 在世：正常决策，不应被守卫拦截（不关心具体返回，只确认不 panic / 不被拦截）
	w.DecideFor(1)

	// 休眠：守卫应拦截
	w.agents[1].Life = life.LifeSleeping
	if act := w.DecideFor(1); act != nil {
		t.Fatalf("sleeping agent should be guarded by LifeState (got %v)", act)
	}

	// 死亡：守卫应拦截
	w.agents[1].Life = life.LifeDead
	if act := w.DecideFor(1); act != nil {
		t.Fatalf("dead agent should be guarded by LifeState (got %v)", act)
	}

	// 回到在世：守卫放行
	w.agents[1].Life = life.LifeAlive
	w.DecideFor(1) // 不应 panic
}
