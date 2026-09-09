package economy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agentworld/internal/life"
)

// TestMoveRemote_Success 验证跨进程 Travel 的“成功路径”：
// 源世界 Export（快照）→ POST 目的地 /enter → 目的地 ACK → 源世界 Commit Leave。
// 完成后：目的地拥有 Marcus，源世界已移除他（不复制、不丢失）。
func TestMoveRemote_Success(t *testing.T) {
	src := newBridgeTestAdapter()
	dst := newBridgeTestAdapter()

	// 先在源世界放入 Marcus（blacksmith Lv5 → economy engineer Lv5）
	marcus := blacksmithPortable()
	if err := src.Enter(marcus); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	id := marcus.AgentID

	// 目的地起一个真实 HTTP Life 端点
	mux := http.NewServeMux()
	life.RegisterEnterHandler(mux, dst, "/life")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tr, err := life.MoveRemote(src, srv.URL+"/life", id)
	if err != nil {
		t.Fatalf("MoveRemote: %v", err)
	}
	if tr.Status != life.TransitionCompleted {
		t.Fatalf("expected completed, got %s (%s)", tr.Status, tr.Err)
	}

	// 目的地已落地 Marcus（稳定身份不变），源世界已移除
	if _, ok := dst.LocalID(id); !ok {
		t.Fatalf("destination should now hold Marcus")
	}
	// Leave 只从源世界 store 移除 Agent（idMap 仍保留 stableID→local 以便同世界重入复用），
	// 因此用 StoreGet 而非 LocalID 判定“源已移除”。
	srcLocal, _ := src.LocalID(id)
	if _, ok := src.StoreGet(srcLocal); ok {
		t.Fatalf("source should have removed Marcus after commit")
	}
	// 目的地内部的技能桥：Marcus 在 economy 里是 engineer（不认 blacksmith）
	dstRaw, _ := dst.StoreGet(mustLocal(t, dst, id))
	dstAg := dstRaw.(*Agent)
	if !dstAg.HasSkill("engineer") {
		t.Fatalf("destination agent should be engineer, got %+v", dstAg.Skills)
	}
}

// TestMoveRemote_Failure 验证“失败回滚路径”：
// 目的地拒绝（500）→ 源世界保持不变（Rollback，Agent 始终留在源世界，绝不凭空消失）。
func TestMoveRemote_Failure(t *testing.T) {
	src := newBridgeTestAdapter()
	marcus := blacksmithPortable()
	if err := src.Enter(marcus); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	id := marcus.AgentID

	// 目的地端点一律返回 500（模拟 Economy 宕机 / 拒绝）
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "economy down", http.StatusInternalServerError)
	}))
	defer fail.Close()

	tr, err := life.MoveRemote(src, fail.URL+"/life", id)
	if err == nil {
		t.Fatalf("expected error on destination failure")
	}
	if tr.Status != life.TransitionFailed {
		t.Fatalf("expected failed, got %s", tr.Status)
	}
	// 源世界必须仍持有 Marcus（回滚：从未移除）
	srcLocal, _ := src.LocalID(id)
	if _, ok := src.StoreGet(srcLocal); !ok {
		t.Fatalf("source must keep Marcus on rollback")
	}
}

func mustLocal(t *testing.T, a *EconomyAdapter, id life.AgentID) int64 {
	local, ok := a.LocalID(id)
	if !ok {
		t.Fatalf("agent %q not found locally", id)
	}
	return local
}
