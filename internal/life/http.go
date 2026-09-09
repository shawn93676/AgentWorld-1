package life

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// remoteSource 是 MoveRemote 对“源世界 adapter”的最小要求：在 PortableAdapter 之上，
// 还需能按稳定 ID 在本地定位 Agent、并能无副作用地快照取出其便携状态。
// 真实的 EconomyAdapter / VillageAdapter 都嵌入了 *BaseAdapter（提供 LocalID / Leave / Export）
// 并自行实现 StoreGet，因此天然满足本接口——无需改动任何公开接口。
type remoteSource interface {
	PortableAdapter
	LocalID(id AgentID) (int64, bool)
	StoreGet(local int64) (any, bool)
}

// RegisterEnterHandler 把一个 PortableAdapter 挂成 HTTP 端点，使本世界可作为跨进程 Travel 的
// “目的地”：POST {prefix}/enter 接收 AgentPortable 并 Enter（落地），GET {prefix}/healthz 探活。
// prefix 通常以 "/life" 之类开头。这样 WorldKey → Endpoint → HTTP World API 的最后一跳就落地了，
// 而 Village / Economy 互相不调用对方代码——只有 Agent → Life Runtime → World。
func RegisterEnterHandler(mux *http.ServeMux, adapter PortableAdapter, prefix string) {
	mux.HandleFunc(prefix+"/enter", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var p AgentPortable
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, "bad portable: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := adapter.Enter(p); err != nil {
			http.Error(w, "enter failed: "+err.Error(), http.StatusConflict)
			return
		}
		writeLifeOK(w, map[string]interface{}{"ok": true, "world": adapter.WorldKey()})
	})
	mux.HandleFunc(prefix+"/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeLifeOK(w, map[string]interface{}{"ok": true, "world": adapter.WorldKey()})
	})
}

func writeLifeOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// PostEnter 把 AgentPortable 发往目的地的 /enter 端点（baseURL 形如 http://host:port/life）。
func PostEnter(baseURL string, p AgentPortable) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal portable: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/enter", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post enter: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("enter rejected (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}

// MoveRemote 跨进程事务性地转移 Agent（WorldKey → Endpoint → HTTP World API）：
//
//	1. from.Export 取出便携状态（不移除，纯快照）
//	2. POST 到 toEndpoint/enter（目的地 Enter）
//	3. 目的地 ACK → from.Leave 提交本地移除
//
// 失败（目的地拒绝 / 网络错误）→ 不移除本地（Rollback，Agent 始终留在源世界，绝不凭空消失）。
// 这样以后换成真正远程服务器，只需改 Endpoint URL，无需改动 Agent 的生命周期模型。
//
// 与同进程 Move 的区别：Move 先 Leave 再 Enter；这里刻意先远程 Enter 成功、再本地 Leave，
// 因为跨进程时“目的地已确认接纳”比“本地先腾退”更安全（远程失败不会留下已丢失的 Agent）。
func MoveRemote(from remoteSource, toEndpoint string, id AgentID) (Transition, error) {
	t := Transition{AgentID: id, From: from.WorldKey(), To: toEndpoint, Status: TransitionPending, At: time.Now()}
	local, ok := from.LocalID(id)
	if !ok {
		t.Status = TransitionFailed
		t.Err = fmt.Sprintf("source %s: agent %q not found", from.WorldKey(), id)
		return t, fmt.Errorf(t.Err)
	}
	raw, ok := from.StoreGet(local)
	if !ok {
		t.Status = TransitionFailed
		t.Err = fmt.Sprintf("source %s: agent %q missing", from.WorldKey(), id)
		return t, fmt.Errorf(t.Err)
	}
	carried, err := from.Export(raw)
	if err != nil {
		t.Status = TransitionFailed
		t.Err = fmt.Sprintf("export %s: %v", from.WorldKey(), err)
		return t, err
	}
	if err := PostEnter(toEndpoint, carried); err != nil {
		// Rollback：源世界从未移除 Agent，状态不变。
		t.Status = TransitionFailed
		t.Err = fmt.Sprintf("enter %s: %v", toEndpoint, err)
		return t, err
	}
	// 目的地 ACK：提交本地移除。
	if _, err := from.Leave(id); err != nil {
		// 已抵达目的地，但本地移除失败——Agent 实际已在新世界，标记 completed 并附带告警。
		t.Status = TransitionCompleted
		t.Err = fmt.Sprintf("commit leave (agent already at destination): %v", err)
		return t, err
	}
	t.Status = TransitionCompleted
	return t, nil
}

// PortableEvent 是跨进程推送的轻量事件（例如 Economy 把“Marcus 完成工作”推回 Village 事件流，
// 使 Village/Event Stream 能看到 departure / work / return 三段旅程）。它不携带任何世界特定逻辑，
// 只描述“谁、在什么、发生了什么”。
type PortableEvent struct {
	Icon  string `json:"icon"`
	Type  string `json:"type"`
	Actor string `json:"actor"`
	Text  string `json:"text"`
	Why   string `json:"why,omitempty"`
}

// PostEvent 把一条事件推送到目的地的 /event 端点（baseURL 形如 http://host:port/life）。
func PostEvent(baseURL string, ev PortableEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/event", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post event: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("event rejected (%d): %s", resp.StatusCode, string(b))
	}
	return nil
}
