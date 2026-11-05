package model

import "time"

// InspectionReport 一次环境巡检的结果（存档）。
//
// 为什么整份 HTML 存库而不是每次用模板重新渲染：
// **存档要保存"当时的样子"**。模板以后会改（加检查项、换配色），
// 而历史报告一旦跟着变，就没法回答"上个月巡检时它到底是什么样"。
// 代价是几十 KB 的存储，换的是可追溯性 —— 值。
//
// 为什么不只存 JSON：JSON 要重新渲染才有意义，而用户下载下来的是一个文件，
// 双击就该能看。HTML 自包含（样式内联），不依赖平台在线。
type InspectionReport struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	// EnvKey 巡检的环境。改名后仍按当时的 env_key 存，便于按环境翻历史。
	EnvKey  string `gorm:"size:64;not null;index" json:"envKey"`
	EnvName string `gorm:"size:128" json:"envName"`
	// Score 0-100 健康分。算法见 service/inspection.go 的 scoreInspection。
	Score int `gorm:"not null" json:"score"`
	// Level healthy / warning / critical
	Level string `gorm:"size:16;not null" json:"level"`
	// Summary 一句话结论（"1 项服务未就绪，节点内存 94%"）。
	Summary string `gorm:"size:255" json:"summary"`
	// FindingsJSON 结构化检查项。留着是为了以后做"上次 vs 这次"对比，
	// 或者让 AI 直接读历史结论 —— 光有 HTML 就只能给人看。
	FindingsJSON JSONText `gorm:"type:json" json:"-"`
	// HTML 彩色报告正文（自包含）。下载直接吐它。
	HTML string `gorm:"type:longtext" json:"-"`
	// Size 报告字节数，列表页显示"多大"（也便于发现渲染异常膨胀）。
	Size int64 `json:"size"`
	// ClusterReadable 集群这次是否读到了。false 时报告里那几节要标"未读到"，
	// 而不是给一堆 0 —— 0 会被读成"资源占用为零"。
	ClusterReadable bool      `gorm:"not null;default:false" json:"clusterReadable"`
	CreatedBy       *uint64   `json:"-"`
	CreatedAt       time.Time `json:"createdAt"`
}

func (InspectionReport) TableName() string { return "inspection_report" }
