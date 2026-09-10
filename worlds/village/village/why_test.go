package village

import (
	"strings"
	"testing"

	"agentworld/worlds/goosegame/goose"
)

func newWhyWorld() *World {
	obs := goose.NewObservatory(goose.ObservOpts{MaxEvents: 100})
	w := NewWorld(obs, "")
	w.now = 1440*10 + 500 // Day 11，越过缓冲期
	w.dailyActs = 10
	a := &Agent{ID: 1, Name: "John", Occupation: "Blacksmith", Place: "market",
		Rel: map[int64]*Relationship{}, Owes: map[int64]int64{}}
	w.agents[1] = a
	w.agentOrd = append(w.agentOrd, 1)
	return w
}

func findWhyLog(entries []WhyLogEntry, kind string) *WhyLogEntry {
	for i := range entries {
		if entries[i].Kind == kind {
			return &entries[i]
		}
	}
	return nil
}

func TestInfluenceAcceptWritesWhyTrace(t *testing.T) {
	w := newWhyWorld()
	a := w.agents[1]
	// 高信任 + 低倔强 → 接受概率被拉满
	pr := playerRel(a)
	pr.Trust = 100
	pr.Like = 100
	a.Grit = 0

	// 接受与否由 rng 决定（p=0.92），循环直到接受以保证确定性（最多 10 次额度足够）。
	var res map[string]interface{}
	for i := 0; i < 10; i++ {
		res = w.Influence(1, "You should open your own shop.")
		if res != nil && res["accepted"].(bool) {
			break
		}
	}
	if res == nil || !res["accepted"].(bool) {
		t.Fatalf("高信任下应接受影响，实际 %v", res)
	}
	// 事件上应携带结构化 Trace（定位到 accept 的那条）
	var ev *Event
	for i := range w.events {
		if w.events[i].Type == "player" && w.events[i].Trace != nil && w.events[i].Trace.Rule == "influence.accept" {
			e := w.events[i]
			ev = &e
		}
	}
	if ev == nil || ev.Trace == nil || ev.Trace.Rule != "influence.accept" {
		t.Fatalf("影响事件应携带 influence.accept 的 Trace")
	}
	causeOK := false
	for _, c := range ev.Trace.Cause {
		if c == "trust=100" {
			causeOK = true
		}
	}
	if !causeOK {
		t.Fatalf("Cause 应含 trust=100，实际 %v", ev.Trace.Cause)
	}
	// WhyLog 中应能查到这条重要决定
	entry := findWhyLog(w.WhyLog(), "influence.accept")
	if entry == nil {
		t.Fatalf("WhyLog 应记录 influence.accept")
	}
	if entry.Decision != "accepted player's influence" {
		t.Fatalf("WhyLog Decision 缺失，实际 %q", entry.Decision)
	}
}

func TestGoalCompletedWritesWhyTrace(t *testing.T) {
	w := newWhyWorld()
	a := w.agents[1]
	a.Goal = "Open my own shop"
	a.GoalTarget = 100
	a.Money = 100 // 工资一发即达标

	w.settleWork(a)

	if !a.GoalDone {
		t.Fatalf("目标应当完成")
	}
	entry := findWhyLog(w.WhyLog(), "goal.completed")
	if entry == nil {
		t.Fatalf("WhyLog 应记录 goal.completed")
	}
	if entry.Rule != "goal.completed" {
		t.Fatalf("Rule 应为 goal.completed，实际 %q", entry.Rule)
	}
	// 事件也应携带 Trace
	found := false
	for _, e := range w.events {
		if e.Type == "milestone" && e.Trace != nil && e.Trace.Rule == "goal.completed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("完成目标的里程碑事件应携带 goal.completed 的 Trace")
	}
}

func TestGoalAbandonedWritesWhyTrace(t *testing.T) {
	w := newWhyWorld()
	a := w.agents[1]
	a.Goal = "Open my own shop"
	a.GoalTarget = 500
	a.Money = 20
	a.Mood = -60 // 跌破 giveUpMood(-55) → 绝望性放弃

	w.checkGoalsLocked()

	if !a.GoalAbandoned {
		t.Fatalf("低心情且远离目标时，应当放弃目标")
	}
	entry := findWhyLog(w.WhyLog(), "goal.abandoned")
	if entry == nil {
		t.Fatalf("WhyLog 应记录 goal.abandoned")
	}
	if entry.Rule != "goal.abandoned" {
		t.Fatalf("Rule 应为 goal.abandoned，实际 %q", entry.Rule)
	}
	// Cause 必须给出触发线与原因
	causeOK, trigOK := false, false
	for _, c := range entry.Cause {
		if c == "money=20 (target 500, gap 480)" {
			causeOK = true
		}
		if c == "triggered_by=hopelessness" {
			trigOK = true
		}
	}
	if !causeOK {
		t.Fatalf("Cause 应给出 money 与缺口，实际 %v", entry.Cause)
	}
	if !trigOK {
		t.Fatalf("Cause 应标注 triggered_by=hopelessness，实际 %v", entry.Cause)
	}
	if !strings.Contains(entry.Decision, "goal abandoned") {
		t.Fatalf("Decision 应说明放弃目标，实际 %q", entry.Decision)
	}
	// 事件也应携带 Trace
	found := false
	for _, e := range w.events {
		if e.Type == "despair" && e.Trace != nil && e.Trace.Rule == "goal.abandoned" {
			found = true
		}
	}
	if !found {
		t.Fatalf("放弃目标的事件应携带 goal.abandoned 的 Trace")
	}
	// 放弃后不应再判定完成
	a.Money = 500
	w.settleWork(a)
	if a.GoalDone {
		t.Fatalf("已放弃的目标不应再被判定为完成")
	}
}

func TestGoalAbandonedStagnationWritesWhyTrace(t *testing.T) {
	w := newWhyWorld()
	a := w.agents[1]
	a.Goal = "Open my own shop"
	a.GoalTarget = 500
	a.Money = 10
	a.goalBest = 100 // 历史最高远高于现状，说明长期无进展
	a.goalStallDays = 40
	a.Mood = 0 // 心情没崩，纯粹是停滞

	w.checkGoalsLocked()

	if !a.GoalAbandoned {
		t.Fatalf("长期停滞时，应当放弃目标")
	}
	entry := findWhyLog(w.WhyLog(), "goal.abandoned")
	if entry == nil {
		t.Fatalf("WhyLog 应记录 goal.abandoned")
	}
	trigOK := false
	for _, c := range entry.Cause {
		if c == "triggered_by=stagnation" {
			trigOK = true
		}
	}
	if !trigOK {
		t.Fatalf("停滞路径应标注 triggered_by=stagnation，实际 %v", entry.Cause)
	}
}

func TestGoalAbandonedRecoversWithFortune(t *testing.T) {
	w := newWhyWorld()
	a := w.agents[1]
	a.Goal = "Open my own shop"
	a.GoalTarget = 500
	a.Money = 20
	a.Mood = -60
	w.checkGoalsLocked()
	if !a.GoalAbandoned {
		t.Fatalf("前置：应当先放弃目标")
	}
	// 一笔横财让他够到目标 → 重燃希望，清除放弃状态
	a.Money = 500
	a.Mood = 10
	w.checkGoalsLocked()
	if a.GoalAbandoned {
		t.Fatalf("够到目标后应当重燃希望、清除放弃状态")
	}
}

func TestWhyLogLastFiltersByDay(t *testing.T) {
	w := newWhyWorld()
	a := w.agents[1]
	a.Goal = "Open my own shop"
	a.GoalTarget = 100
	a.Money = 100
	w.settleWork(a) // Day 11 记录一条

	// Day 11 落在"最近 7 天"窗口内（11..17），应保留
	if n := len(w.WhyLogLast(7)); n != 1 {
		t.Fatalf("Day 11 应在 7 天窗口内，实际 %d", n)
	}
	// 推进到远超窗口的第 41 天，应被过滤掉
	w.now = 1440*40 + 500 // Day 41
	if n := len(w.WhyLogLast(7)); n != 0 {
		t.Fatalf("远超窗口时应为 0，实际 %d", n)
	}
}
