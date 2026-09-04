package models

import "time"

// RealWorldJob 真实世界任务（M8 Real Economy）。
//
// 设计铁律：本对象只描述“真实需求”，与模拟经济（economy 世界的 coins）完全隔离。
// 真实结算走 RealWorldPayment（收入账本），绝不反向写入 economy 余额，
// 避免“虚拟 Coins 与真实收入绑死”的架构债务。
type RealWorldJob struct {
	ID                 int64      `json:"id" gorm:"primaryKey"`
	Title              string     `json:"title" gorm:"type:varchar(200)"`
	Description        string     `json:"description" gorm:"type:text"`
	Reward             float64    `json:"reward"`
	Currency           string     `json:"currency" gorm:"type:varchar(8);default:USD"`
	Deadline           *time.Time `json:"deadline,omitempty"`
	RequiredSkill      string     `json:"required_skill" gorm:"type:varchar(64);index"`
	AcceptanceCriteria string     `json:"acceptance_criteria" gorm:"type:text"`
	Status             string     `json:"status" gorm:"type:varchar(16);default:OPEN;index"`
	Customer           string     `json:"customer" gorm:"type:varchar(120)"`
	PaymentStatus      string     `json:"payment_status" gorm:"type:varchar(16);default:unpaid"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// RealWorldExecution Agent 接单后的真实执行记录。
// JobID 作为主键：每个任务仅保留一份最新执行（REJECTED 后重做会覆盖）。
type RealWorldExecution struct {
	JobID       int64     `json:"job_id" gorm:"primaryKey"`
	AgentID     int64     `json:"agent_id"`
	AgentName   string    `json:"agent_name" gorm:"type:varchar(120)"`
	Workspace   string    `json:"workspace" gorm:"type:varchar(512)"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	OutputFiles []string  `json:"output_files" gorm:"serializer:json"`
	Logs        string    `json:"logs" gorm:"type:text"`
	Result      string    `json:"result" gorm:"type:text"` // JSON 字符串
	Success     bool      `json:"success"`
}

// RealWorldPayment 真实结算记录（即收入账本）。
// 这是“真实收入”的唯一事实来源，与任何模拟经济账户无关。
type RealWorldPayment struct {
	JobID         int64     `json:"job_id" gorm:"primaryKey"`
	Amount        float64   `json:"amount"`
	Currency      string    `json:"currency" gorm:"type:varchar(8)"`
	Provider      string    `json:"provider" gorm:"type:varchar(64)"` // paypal / stripe / crypto / manual ...
	TransactionID string    `json:"transaction_id" gorm:"type:varchar(128)"`
	Status        string    `json:"status" gorm:"type:varchar(16);default:paid"`
	CreatedAt     time.Time `json:"created_at"`
}
