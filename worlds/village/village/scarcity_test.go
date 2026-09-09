package village

import (
	"testing"

	"agentworld/worlds/goosegame/goose"
)

func newTestWorld() *World {
	obs := goose.NewObservatory(goose.ObservOpts{MaxEvents: 100})
	w := NewWorld(obs, "")
	// 推进到缓冲期之后（Day 11），让离村判定能够触发
	w.now = 1440*10 + 500
	return w
}

func TestQuotaExhausts(t *testing.T) {
	w := newTestWorld()
	w.dailyActs = 2
	if q, ok := w.spendActLocked(); !ok || q.Left != 1 {
		t.Fatalf("第一次应成功，left=1，实际 ok=%v left=%d", ok, q.Left)
	}
	if q, ok := w.spendActLocked(); !ok || q.Left != 0 {
		t.Fatalf("第二次应成功，left=0，实际 ok=%v left=%d", ok, q.Left)
	}
	if _, ok := w.spendActLocked(); ok {
		t.Fatalf("第三次应被拒绝（额度用尽）")
	}
}

func TestQuotaResetsAcrossDay(t *testing.T) {
	w := newTestWorld()
	w.dailyActs = 1
	w.spendActLocked()
	if w.quotaLocked().Left != 0 {
		t.Fatalf("当日额度应为 0")
	}
	w.player.ActDay = -1 // 假装是另一天
	if w.quotaLocked().Left != 1 {
		t.Fatalf("跨日应重置为 1，实际 %d", w.quotaLocked().Left)
	}
}

func TestDepartureWritesOffDebt(t *testing.T) {
	w := newTestWorld()
	for i := int64(1); i <= 4; i++ {
		a := &Agent{ID: i, Name: "P" + string(rune('0'+i)), Place: "Home", Rel: map[int64]*Relationship{}, Owes: map[int64]int64{}}
		w.agents[i] = a
		w.agentOrd = append(w.agentOrd, i)
	}
	// P1 深陷债务与绝望，应当离开
	w.agents[1].Money = -40
	w.agents[1].Mood = -90
	w.agents[1].Owes[2] = 50 // 欠 P2 50 金

	w.checkFatesLocked()

	if !w.agents[1].Gone {
		t.Fatalf("P1 应当离开村庄")
	}
	// 债务作废：P2 不再被欠，且 P2 记下了损失
	if _, stillOwed := w.agents[1].Owes[2]; stillOwed {
		t.Fatalf("离村后债务应当清除")
	}
	found := false
	for _, e := range w.events {
		if e.Type == "gone" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应当产生一条 gone 事件")
	}
}
