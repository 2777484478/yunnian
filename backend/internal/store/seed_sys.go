package store

import (
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/model"
)

// 通知渠道种子数据。
//
// ⚠️ 2026-10-10 收敛：**只种平台真的发得出去的渠道类型**。
//
// 原先这里还有 `wecom` 与 `webhook` 两条 —— 而这两种发送器**根本不存在**：
// `TestNotifyChannel` 会明确回"尚未接入发送器"，编辑时又会被 `oneof=email dingtalk`
// 拒掉。用户看到的是一条能出现在列表里、点开编辑必然失败、点测试必然报错的渠道。
// **配得出来却发不出去，就是安静的假承诺。** 其中 webhook 那条更糟：它
// `enabled=true` 且订阅 `build_failed` —— 每次构建失败都会去投递、每次必然失败，
// 把 `fail_count` 一路加上去（不是"躺着不动的假数据"，是个会响的坏渠道）。
//
// 加回来很便宜：写一个发送器 + 把类型加进 `service.NotifyInput.Type` 的 oneof。
//
// `events` 同样只种**真的会触发**的三个码（与 `service.SupportedNotifyEvents` 一致）。
// 原种子里的 `env_unhealthy` / `deploy_success` / `deploy_approval` / `rollback_executed`
// 没有任何代码会触发 —— 勾上、存库、看到勾上了，然后永远收不到。
//
// 注：dingtalk / email 的 Target 仍是 `****` 占位（真实地址属凭据，不入库到种子），
// 装完要由使用者在「系统管理 › 通知渠道」里填真实值。
var notifySeed = []struct {
	Name, Type, Target string
	Events             string
	Enabled            bool
}{
	// 两条种子的事件必须与 `SupportedNotifyEvents` 对齐：
	// 少订阅一个真事件 = 全新部署上线就带着一个告警缺口（平台重启打断的任务没人知道）；
	// 多订阅一个假事件 = 用户勾了一个永远不会响的框。
	// `seed_notify_channels_test.go` 会对这两件事同时报红。
	{"运维告警群", "dingtalk", "https://oapi.dingtalk.com/robot/send?access_token=****", `["build_failed","build_interrupted","deploy_failed","deploy_interrupted","deploy_pending_approval"]`, true},
	{"值班邮箱", "email", "ops-oncall@yunnian.cn", `["build_failed","build_interrupted","deploy_failed","deploy_interrupted","deploy_pending_approval"]`, true},
}

// configSeed 系统配置，逐条来自 docs/02 的 system_config 初始化数据。
var configSeed = []struct {
	Key, Value, Type, Group, Desc string
}{
	{"build.timeout.default", "1800", "number", "build", "默认构建超时（秒）"},
	{"build.concurrent", "4", "number", "build", "Runner 并发构建数"},
	{"build.log.max.size", "52428800", "number", "build", "单次构建日志上限（字节）"},
	{"build.retain.images", "30", "number", "image", "每项目保留镜像版本数"},
	{"build.retain.days", "90", "number", "image", "镜像保留天数"},
	{"deploy.timeout", "600", "number", "deploy", "部署超时（秒）"},
	{"deploy.lock.ttl", "1800000", "number", "deploy", "部署互斥锁 TTL（毫秒）"},
	{"audit.retain.days", "365", "number", "audit", "审计日志保留天数"},
	{"notify.retry.times", "3", "number", "notify", "通知失败重试次数"},
	{"ai.enabled", "true", "bool", "ai", "AI 能力开关"},
}

// modelProviderSeed 大模型通道，与前端「系统管理 › 模型设置」预置的四个通道一致。
type modelProviderSeed struct {
	Name, Vendor, BaseURL, KeyMasked, Models, DefaultModel, Status, Desc string
	Enabled, IsDefault                                                   bool
	Timeout, MaxConcurrency, MaxTokens                                   int
	Temperature                                                          float64
	CallCount, FailCount                                                 uint32
	LatencyMS                                                            int
}

var modelProviderSeeds = []modelProviderSeed{
	{
		Name: "阿里云百炼", Vendor: "dashscope",
		BaseURL:      "https://dashscope.aliyuncs.com/compatible-mode/v1",
		KeyMasked:    "sk-****************a3f9",
		Models:       `["qwen-plus","qwen-max","qwen-turbo","qwen2.5-72b-instruct"]`,
		DefaultModel: "qwen-plus", Status: "connected",
		Desc:    "OpenAI 兼容模式，平台默认通道，适用于对话式运维与日志归因",
		Enabled: true, IsDefault: true,
		Timeout: 60, MaxConcurrency: 3, MaxTokens: 2048, Temperature: 0.7,
		CallCount: 1842, FailCount: 12, LatencyMS: 486,
	},
	{
		Name: "通义千问", Vendor: "qwen",
		BaseURL:      "https://dashscope.aliyuncs.com/api/v1",
		KeyMasked:    "sk-****************a3f9",
		Models:       `["qwen-max","qwen-plus"]`,
		DefaultModel: "qwen-max", Status: "connected",
		Desc:    "原生 DashScope 协议，长文本分析场景使用",
		Enabled: true,
		Timeout: 90, MaxConcurrency: 2, MaxTokens: 4096, Temperature: 0.5,
		CallCount: 236, FailCount: 3, LatencyMS: 612,
	},
	{
		Name: "DeepSeek", Vendor: "deepseek",
		BaseURL:      "https://api.deepseek.com/v1",
		KeyMasked:    "sk-****************7c1e",
		Models:       `["deepseek-chat","deepseek-reasoner"]`,
		DefaultModel: "deepseek-chat", Status: "connected",
		Desc:    "代码与日志分析性价比高，用于构建失败归因",
		Enabled: true,
		Timeout: 60, MaxConcurrency: 3, MaxTokens: 2048, Temperature: 0.7,
		CallCount: 764, FailCount: 5, LatencyMS: 728,
	},
	{
		Name: "内网 vLLM（自建）", Vendor: "custom",
		BaseURL:      "http://10.8.12.31:8000/v1",
		KeyMasked:    "未配置",
		Models:       `["Qwen2.5-32B-Instruct"]`,
		DefaultModel: "Qwen2.5-32B-Instruct", Status: "failed",
		Desc:    "内网自建推理服务，数据不出内网；当前实例未启动",
		Enabled: false,
		Timeout: 120, MaxConcurrency: 1, MaxTokens: 4096, Temperature: 0.7,
		CallCount: 0, FailCount: 0, LatencyMS: 0,
	},
}

// auditSeed 审计日志模板。
var auditSeed = []struct {
	Module, Action, TargetType, Detail string
	Result                             string
}{
	{"认证", "登录控制台", "用户", "账号密码登录成功", "success"},
	{"认证", "退出登录", "用户", "主动退出登录", "success"},
	{"认证", "登录控制台", "用户", "密码错误，登录失败", "failed"},
	{"CI", "触发构建", "项目", "手动触发 master 分支构建", "success"},
	{"CI", "取消构建", "项目", "构建排队超时，操作人取消", "success"},
	{"CI", "批量打包", "项目", "批量触发 6 个微服务构建", "success"},
	{"CI", "维护项目配置", "项目", "更新构建参数与 Tag 规则", "success"},
	{"CD", "执行部署", "服务", "发布新版本到目标环境", "success"},
	{"CD", "审批部署", "部署单", "审批通过正式区发布申请", "success"},
	{"CD", "驳回部署", "部署单", "版本变更说明不完整，驳回", "failed"},
	{"CD", "批量发布", "服务", "批量发布 5 个微服务", "success"},
	{"CD", "执行回滚", "服务", "因接口错误率上升回滚到上一稳定版本", "success"},
	{"CD", "扩缩容", "服务", "副本数由 2 调整为 4", "success"},
	{"CD", "维护环境", "环境", "更新环境级变量", "success"},
	{"系统", "新增通知渠道", "通知渠道", "新增钉钉运维告警群", "success"},
	{"系统", "修改模型通道", "AI 通道", "更新阿里云百炼的默认模型", "success"},
	{"系统", "连通性测试", "AI 通道", "内网 vLLM 连通性测试失败", "failed"},
	{"系统", "停用用户", "用户", "离职账号已停用", "success"},
}

func (s *seeder) system(tx *gorm.DB) error {
	s.init()

	// ------------------------------------------------------------------ 通知渠道
	channels := make([]model.NotifyChannel, 0, len(notifySeed))
	for _, n := range notifySeed {
		channels = append(channels, model.NotifyChannel{
			Name:      n.Name,
			Type:      n.Type,
			Target:    n.Target,
			Events:    model.JSONText(n.Events),
			Enabled:   n.Enabled,
			UpdatedAt: s.now.AddDate(0, 0, -s.rnd.Intn(30)),
		})
	}
	if err := tx.Create(&channels).Error; err != nil {
		return err
	}

	// ------------------------------------------------------------------ 系统配置
	configs := make([]model.SystemConfig, 0, len(configSeed))
	for _, c := range configSeed {
		configs = append(configs, model.SystemConfig{
			ConfigKey:   c.Key,
			ConfigValue: c.Value,
			ValueType:   c.Type,
			GroupName:   c.Group,
			Description: c.Desc,
			UpdatedAt:   s.now.AddDate(0, 0, -s.rnd.Intn(60)),
		})
	}
	if err := tx.Create(&configs).Error; err != nil {
		return err
	}

	// ------------------------------------------------------------------ AI 模型通道
	models := make([]model.AIModelConfig, 0, len(modelProviderSeeds))
	for _, m := range modelProviderSeeds {
		checked := s.now.Add(-time.Duration(10+s.rnd.Intn(600)) * time.Minute)
		rec := model.AIModelConfig{
			Name:           m.Name,
			Vendor:         m.Vendor,
			BaseURL:        m.BaseURL,
			APIKeyMasked:   m.KeyMasked,
			Models:         model.JSONText(m.Models),
			DefaultModel:   m.DefaultModel,
			Enabled:        m.Enabled,
			IsDefault:      m.IsDefault,
			TimeoutSec:     m.Timeout,
			MaxConcurrency: m.MaxConcurrency,
			Temperature:    m.Temperature,
			MaxTokens:      m.MaxTokens,
			Status:         m.Status,
			CheckedAt:      &checked,
			CallCount:      m.CallCount,
			FailCount:      m.FailCount,
			Description:    m.Desc,
			UpdatedAt:      s.now.AddDate(0, 0, -s.rnd.Intn(14)),
		}
		if m.LatencyMS > 0 {
			rec.LatencyMS = &m.LatencyMS
		}
		models = append(models, rec)
	}
	if err := tx.Create(&models).Error; err != nil {
		return err
	}

	// ------------------------------------------------------------------ 审计日志
	// 生成最近 7 天内的操作记录，按时间倒序铺开。
	logs := make([]model.AuditLog, 0, 96)
	operators := []string{"系统管理员", "运维示例", "张伟", "李静", "曹阳", "冯雪", "韩磊"}
	for i := 0; i < 96; i++ {
		tpl := auditSeed[s.rnd.Intn(len(auditSeed))]
		name := operators[s.rnd.Intn(len(operators))]
		at := s.now.Add(-time.Duration(s.rnd.Intn(7*24*60)) * time.Minute)

		result := tpl.Result
		// 让失败态不至于过多：模板为 failed 时只有一半概率真的失败
		if result == "failed" && s.rnd.Intn(2) == 0 {
			result = "success"
		}

		log := model.AuditLog{
			Username:   "u" + fmt.Sprint(1000+s.rnd.Intn(9000)),
			UserName:   name,
			RoleCode:   roleOfSeedName(name),
			Module:     tpl.Module,
			Action:     tpl.Action,
			TargetType: tpl.TargetType,
			TargetName: s.auditTarget(tpl.Module, tpl.Action),
			Detail:     tpl.Detail,
			Result:     result,
			IP:         fmt.Sprintf("10.20.31.%d", 20+s.rnd.Intn(60)),
			UserAgent:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/128.0",
			TraceID:    s.randHex(8),
			CreatedAt:  at,
		}
		if uid, ok := s.userIDs[name]; ok {
			log.UserID = &uid
		}
		if result == "failed" {
			log.ErrorMsg = "操作未通过校验"
		}
		logs = append(logs, log)
	}
	return tx.Create(&logs).Error
}

// auditTarget 给审计记录编一个具体的操作对象，让列表看起来是真实发生的操作。
func (s *seeder) auditTarget(module, action string) string {
	switch module {
	case "CD":
		env := envSpecs[s.rnd.Intn(len(envSpecs))]
		sp := projectSpecs[s.rnd.Intn(len(projectSpecs))]
		return fmt.Sprintf("%s · %s", sp.Name, env.Name)
	case "CI":
		sp := projectSpecs[s.rnd.Intn(len(projectSpecs))]
		return fmt.Sprintf("%s #%d", sp.Name, 60+s.rnd.Intn(1200))
	case "系统":
		items := []string{"运维告警群", "阿里云百炼", "访客账号", "内网 vLLM（自建）"}
		return items[s.rnd.Intn(len(items))]
	default:
		return "运营后台"
	}
}

func roleOfSeedName(name string) string {
	switch name {
	case "系统管理员":
		return "admin"
	case "运维示例":
		return "ops"
	case "李静":
		return "qa"
	default:
		return "dev"
	}
}
