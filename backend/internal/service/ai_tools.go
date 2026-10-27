package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/llm"
	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- AI 只读工具
//
// 为什么要有这一层：在接入工具之前，AI 助手只能拿一份**提问前拼好的事实快照**
// 回答。用户在聊天框里问「测试区192 环境的 demouser 状态」，快照里既没有
// 环境也没有项目（那句话不在任何页面上下文里），于是模型只能如实回答
// 「当前上下文没有这项数据」—— 功能没坏，但它答的不是用户问的。
//
// 加工具之后，模型的回答路径变成：先 list_environments / list_projects 把口语
// 对上标识，再 get_service_status 去**集群**读一次实时状态。这正是"真的接了 AI"
// 与"只是接了模型"的区别：模型有没有能力**自己取数**。
//
// 三条硬约束（与 docs/03 §12 对应）：
//
//  1. **只读，且是结构性的只读**。下面 8 个工具的 Run 里没有一处
//     Apply / Scale / Restart / Rollback。模型就算在正文里写"我帮你重启了"，
//     也没有那个函数可调——安全边界靠接口不存在，不靠提示词。
//  2. **不需要把集群密钥交给模型**。工具在**平台进程内**执行，用的是平台自己
//     那套 K8s 客户端（环境自带 kubeconfig / 集群接入凭据，见 KubeconfigSource）。
//     模型看到的只是返回值。把 kubeconfig 塞进提示词是另一种做法，但它等于
//     把"能改集群的凭证"交给一段可能被提示词注入操纵的文本，不划算。
//  3. **失败也是答案**。工具查不到时回灌 `ok=false` + 原因，由模型如实转述；
//     绝不能悄悄回一个空对象 —— 那会被模型读成"集群里没有异常"，
//     与事实正好相反（这条与 FailureEvidence.Notes 是同一个道理）。

// aiTool 一个可供模型调用的只读工具。
type aiTool struct {
	Name        string
	Description string
	// Parameters JSON Schema；nil 表示无入参。
	Parameters map[string]any
	Run func(ctx context.Context, c Caller, args map[string]any) (string, error)
}

// aiToolDecls 转成给模型看的工具声明。
func (a *AIService) aiToolDecls() []llm.Tool {
	all := a.aiTools()
	out := make([]llm.Tool, 0, len(all))
	for _, t := range all {
		params := t.Parameters
		if params == nil {
			// 无入参的工具也要给 `{}`，不能留 null：网关对
			// tools[].function.parameters 为 null 会直接判 400
			// （这正是内部自检 TestAITools_DeclsCarrySchema 抓到的第一个缺陷）。
			// 收口在这里而不是每个工具里各写一次 —— 加新工具的人不该需要记得这件事。
			params = schema(nil, nil)
		}
		out = append(out, llm.Tool{
			Type: "function",
			Function: llm.ToolDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// findTool 按名字取工具。
func (a *AIService) findTool(name string) *aiTool {
	for _, t := range a.aiTools() {
		if t.Name == name {
			t := t
			return &t
		}
	}
	return nil
}

// aiTools 工具清单。
func (a *AIService) aiTools() []aiTool {
	return []aiTool{
		a.toolListEnvironments(),
		a.toolListProjects(),
		a.toolListEnvServices(),
		a.toolGetServiceStatus(),
		a.toolDiagnoseService(),
		a.toolListDeployments(),
		a.toolListBuilds(),
		a.toolClusterResources(),
	}
}

// --------------------------------------------------------------------------- 名录类

/**
 * list_environments（并**只列出当前用户能看的环境**）。
 *
 * ⚠️ 这个过滤是必须的：有 `ai:query` 的人能通过它读到全部环境的名录，
 * 再拿 env_key 去调别的工具 —— 如果名录不收敛，"环境按权限可见"就形同虚设。
 * 可见性规则走 Caller.VisibleEnvKeys，与接口层**同一份**判断，不另写一套。
 */
func (a *AIService) toolListEnvironments() aiTool {
	return aiTool{
		Name: "list_environments",
		Description: "列出平台纳管的全部环境：env_key（后续所有工具要用的标识）、名称、集群、命名空间、" +
			"集群资源水位（字段名 healthStatus，**只表示该集群节点的 CPU/内存用量**，越线不等于服务故障——" +
			"服务健康要用 get_service_status 去集群读）、是否需要审批。" +
			"用户用口语描述环境时（「测试区192」「正式区」「stage」），先用它把描述对应到 env_key，不要凭猜测填。",
		Run: func(ctx context.Context, c Caller, _ map[string]any) (string, error) {
			q := a.db(ctx)
			// ⚠️ 名录必须跟着可见性收敛：这是模型"知道有哪些环境"的唯一来源，
			// 它直接决定模型会不会去查正式区。过滤规则与接口层同一份（VisibleEnvKeys）。
			if allowed, restricted := c.VisibleEnvKeys(); restricted {
				if len(allowed) == 0 {
					q = q.Where("1 = 0")
				} else {
					q = q.Where("env_key IN ?", allowed)
				}
			}
			var envs []model.Environment
			if err := q.Order("stage_order asc, id asc").Find(&envs).Error; err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(envs))
			for _, e := range envs {
				out = append(out, map[string]any{
					"envKey":       e.EnvKey,
					"name":         e.Name,
					"cluster":      e.Cluster,
					"namespace":    e.Namespace,
					"healthStatus": e.HealthStatus,
					"needApproval": e.NeedApproval,
					"enabled":      e.Enabled,
					"description":  e.Description,
				})
			}
			return jsonText(map[string]any{"ok": true, "environments": out})
		},
	}
}

/**
 * assertToolEnvVisible 工具侧的可见性断言。
 *
 * 与接口层 `CDService.AssertEnvVisible` **同一条规则**（都走 VisibleEnvKeys），
 * 这里是另一份实现而不是复用 —— 因为 AIService 拿不到 CDService，
 * 而为了一个十行的判断去建立依赖不值得。规则本身只有一处定义（VisibleEnvKeys），
 * 所以不会分家。
 */
func assertToolEnvVisible(c Caller, envKey string) error {
	allowed, restricted := c.VisibleEnvKeys()
	if !restricted || envKey == "" {
		return nil
	}
	for _, k := range allowed {
		if k == envKey {
			return nil
		}
	}
	return fmt.Errorf("无权查看环境 %s", envKey)
}

func (a *AIService) toolListProjects() aiTool {
	return aiTool{
		Name: "list_projects",
		Description: "列出平台纳管的 CI 项目（project_key / 名称 / 类型 / 团队），可按关键词模糊过滤。" +
			"用户提到的服务名要先用它确认准确的 project_key —— 注意标识常与口语不一致（如「镜像打包-demoitms」的标识是 demoitms）。",
		Parameters: schema(map[string]any{
			"keyword": map[string]any{"type": "string", "description": "可选，按名称或标识模糊匹配"},
		}, nil),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			q := a.db(ctx).Model(&model.Project{})
			if kw := argStr(args, "keyword"); kw != "" {
				like := "%" + kw + "%"
				q = q.Where("project_key LIKE ? OR name LIKE ?", like, like)
			}
			var ps []model.Project
			if err := q.Order("project_key asc").Limit(50).Find(&ps).Error; err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(ps))
			for _, p := range ps {
				out = append(out, map[string]any{
					"projectKey": p.ProjectKey,
					"name":       p.Name,
					"type":       p.Type,
					"team":       p.Team,
					"enabled":    p.Enabled,
				})
			}
			return jsonText(map[string]any{"ok": true, "projects": out})
		},
	}
}

// --------------------------------------------------------------------------- 平台侧状态

func (a *AIService) toolListEnvServices() aiTool {
	return aiTool{
		Name: "list_env_services",
		Description: "列出某个环境下全部服务的**平台登记**状态（镜像 tag、期望副本、就绪副本、状态）。" +
			"适合回答「这个环境里有哪些服务不正常」这类**面**上的问题。" +
			"注意：数据来自平台数据库，可能滞后于集群；要某个服务的集群实时状态请改用 get_service_status。",
		Parameters: schema(map[string]any{
			"envKey": map[string]any{"type": "string", "description": "环境的 env_key，先用 list_environments 确认"},
		}, []string{"envKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey := argStr(args, "envKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" {
				return "", errors.New("缺少 envKey")
			}
			if _, err := a.aiEnvByKey(ctx, envKey); err != nil {
				return jsonText(failResult(err))
			}
			var svcs []model.ServiceInstance
			if err := a.db(ctx).Where("env_key = ?", envKey).
				Order("project_key asc").Limit(50).Find(&svcs).Error; err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(svcs))
			abnormal := 0
			for _, s := range svcs {
				if s.Status != "running" {
					abnormal++
				}
				out = append(out, map[string]any{
					"projectKey":    s.ProjectKey,
					"tag":           s.CurrentTag,
					"replicas":      s.Replicas,
					"readyReplicas": s.ReadyReplicas,
					"status":        s.Status,
					"updatedAt":     formatTimePtr(&s.UpdatedAt),
				})
			}
			return jsonText(map[string]any{
				"ok": true, "envKey": envKey,
				"source":   "平台数据库（非集群实时）",
				"total":    len(out),
				"abnormal": abnormal,
				"services": out,
			})
		},
	}
}

func (a *AIService) toolGetServiceStatus() aiTool {
	return aiTool{
		Name: "get_service_status",
		Description: "查某个服务在某个环境的**集群实时**状态：工作负载是否存在、当前镜像 tag、期望副本 / 实际存在实例 / 就绪副本，" +
			"以及平台侧最近一次部署单。回答「某服务现在健康吗 / 什么状态 / 有几个副本」必须用这个，" +
			"不要用 list_env_services 的登记值代替。",
		Parameters: schema(map[string]any{
			"envKey":     map[string]any{"type": "string", "description": "环境的 env_key"},
			"projectKey": map[string]any{"type": "string", "description": "项目的 project_key"},
		}, []string{"envKey", "projectKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey, projectKey := argStr(args, "envKey"), argStr(args, "projectKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" || projectKey == "" {
				return "", errors.New("缺少 envKey 或 projectKey")
			}
			env, err := a.aiEnvByKey(ctx, envKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			workload, linked, err := a.aiWorkloadOf(ctx, envKey, projectKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			res := map[string]any{
				"ok": true, "envKey": envKey, "projectKey": projectKey,
				"cluster": env.Cluster, "namespace": env.Namespace,
				"workload": workload, "linked": linked,
			}
			if !linked {
				// 这条必须给：否则模型/用户会把"集群里存在但没纳管"读成
				// "它是本环境的正常服务"—— 两者在运维上处置完全不同。
				res["linkedNote"] = "该服务**没有纳管到本环境**：下面是用 project_key 当工作负载名" +
					"去集群里查到的结果（平台约定两者同名）。若 exists=true，说明集群里确实有这个工作负载，" +
					"但平台不跟踪它 —— 可能是在集群里手工部署的，不受本平台的发布管理。"
			}
			state, err := a.deps.Up.Cluster.Describe(ctx, workloadRefOf(env, workload))
			if err != nil {
				// 「没读到」必须与「不存在」分开说：混为一谈会让用户以为集群里没这个服务。
				res["clusterReadError"] = err.Error()
				res["note"] = "集群这次**没读到**（权限 / 网络 / 超时），下面不给字段值；" +
					"这不代表集群里没有该工作负载。"
			} else {
				res["exists"] = state.Exists
				res["image"] = state.Image
				res["tag"] = state.Tag
				res["replicas"] = state.Replicas
				res["actualReplicas"] = state.ActualReplicas
				res["readyReplicas"] = state.ReadyReplicas
				res["revision"] = state.Revision
				if state.Exists && state.Replicas != state.ActualReplicas {
					// 实测 demobasic：spec=1 / status=2 / ready=1 —— 滚动更新卡住时
					// 旧 Pod 不会被杀，只报期望数会写成「1/1 正常」，把崩溃的那一个抹掉。
					res["replicaNote"] = fmt.Sprintf(
						"期望 %d 个但集群里实际有 %d 个实例，且只有 %d 个就绪 —— 多出来的通常是没能被回收的旧实例，说明有实例在反复重启。",
						state.Replicas, state.ActualReplicas, state.ReadyReplicas)
				}
			}
			if last, ok := a.lastDeployOf(ctx, envKey, projectKey); ok {
				res["lastDeploy"] = last
			}
			return jsonText(res)
		},
	}
}

func (a *AIService) toolDiagnoseService() aiTool {
	return aiTool{
		Name: "diagnose_service",
		Description: "对某个服务的**集群侧故障取证**：未就绪 Pod 的容器状态（CrashLoopBackOff / OOMKilled / ImagePullBackOff 等）、" +
			"退出码、kubelet 原文说明、Pod 事件，以及容器崩溃前那一轮的日志尾部。" +
			"回答「为什么起不来 / 部署失败了怎么办」用它取原始证据，不要凭状态名猜原因。",
		Parameters: schema(map[string]any{
			"envKey":     map[string]any{"type": "string", "description": "环境的 env_key"},
			"projectKey": map[string]any{"type": "string", "description": "项目的 project_key"},
		}, []string{"envKey", "projectKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey, projectKey := argStr(args, "envKey"), argStr(args, "projectKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" || projectKey == "" {
				return "", errors.New("缺少 envKey 或 projectKey")
			}
			env, err := a.aiEnvByKey(ctx, envKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			workload, linked, err := a.aiWorkloadOf(ctx, envKey, projectKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			ev, err := a.deps.Up.Cluster.DiagnoseFailure(ctx, workloadRefOf(env, workload))
			if err != nil {
				return jsonText(failResult(fmt.Errorf("集群侧取证失败（权限 / 网络 / 超时）：%w", err)))
			}
			pods := make([]map[string]any, 0, len(ev.Pods))
			for _, p := range ev.Pods {
				pod := map[string]any{
					"name": p.Name, "phase": p.Phase, "container": p.Container,
					"restarts": p.Restarts, "reason": p.Reason,
					"message": p.Message, "exitCode": p.ExitCode,
				}
				if strings.TrimSpace(p.Logs) != "" {
					pod["logsFrom"] = p.LogsFrom
					pod["logsTail"] = tailLines(p.Logs, 40)
				}
				pods = append(pods, pod)
			}
			res := map[string]any{
				"ok": true, "envKey": envKey, "projectKey": projectKey,
				"workload": workload, "linked": linked,
				"notReadyPods": pods, "events": ev.Events,
			}
			if !linked {
				res["linkedNote"] = "该服务没有纳管到本环境，这里是用 project_key 当工作负载名查到的集群侧证据；" +
					"它不受本平台的发布管理。"
			}
			if len(ev.Notes) > 0 {
				res["notes"] = ev.Notes
			}
			if len(pods) == 0 {
				res["hint"] = "没有未就绪的 Pod：这次问题更像平台侧等待超时，而不是容器起不来。"
			}
			return jsonText(res)
		},
	}
}

// --------------------------------------------------------------------------- 历史记录

func (a *AIService) toolListDeployments() aiTool {
	return aiTool{
		Name: "list_deployments",
		Description: "查部署单记录（部署单号 / 项目 / 镜像 tag / 状态 / 操作人 / 时间）。" +
			"可只给 envKey、只给 projectKey，或两者都给；看「最近一次部署」「谁发的」用它。",
		Parameters: schema(map[string]any{
			"envKey":     map[string]any{"type": "string", "description": "可选"},
			"projectKey": map[string]any{"type": "string", "description": "可选"},
			"limit":      map[string]any{"type": "integer", "description": "返回条数，默认 5，最多 20"},
		}, nil),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey, projectKey := argStr(args, "envKey"), argStr(args, "projectKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" && projectKey == "" {
				return "", errors.New("envKey 与 projectKey 至少要给一个")
			}
			q := a.db(ctx).Model(&model.DeployRecord{})
			if envKey != "" {
				q = q.Where("env_key = ?", envKey)
			}
			if projectKey != "" {
				q = q.Where("project_key = ?", projectKey)
			}
			var ds []model.DeployRecord
			if err := q.Order("id desc").Limit(argInt(args, "limit", 5, 1, 20)).Find(&ds).Error; err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(ds))
			for _, d := range ds {
				out = append(out, map[string]any{
					"deployNo": d.DeployNo, "envKey": d.EnvKey, "projectKey": d.ProjectKey,
					"tag": tagOf(d.Image), "image": d.Image, "status": d.Status,
					"operator": d.OperatorName, "createdAt": formatTime(d.CreatedAt),
				})
			}
			return jsonText(map[string]any{"ok": true, "deployments": out})
		},
	}
}

func (a *AIService) toolListBuilds() aiTool {
	return aiTool{
		Name: "list_builds",
		Description: "查某个项目的构建记录（构建号 / 分支 / 提交 / 状态 / 阶段）。" +
			"回答「构建为什么失败」「近几次构建情况」时先用它定位是哪一次，再结合部署记录与 diagnose_service 分析。",
		Parameters: schema(map[string]any{
			"projectKey": map[string]any{"type": "string", "description": "项目的 project_key"},
			"limit":      map[string]any{"type": "integer", "description": "返回条数，默认 5，最多 20"},
		}, []string{"projectKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			projectKey := argStr(args, "projectKey")
			if projectKey == "" {
				return "", errors.New("缺少 projectKey")
			}
			var bs []model.BuildRecord
			if err := a.db(ctx).Where("project_key = ?", projectKey).
				Order("id desc").Limit(argInt(args, "limit", 5, 1, 20)).Find(&bs).Error; err != nil {
				return "", err
			}
			out := make([]map[string]any, 0, len(bs))
			for _, b := range bs {
				item := map[string]any{
					"buildNo": b.BuildNo, "branch": b.Branch, "commit": shortCommit(b.CommitID),
					"status": b.Status, "stage": b.Stage, "createdAt": formatTime(b.CreatedAt),
				}
				if strings.TrimSpace(b.ErrorCode) != "" {
					item["errorCode"] = b.ErrorCode
				}
				out = append(out, item)
			}
			return jsonText(map[string]any{"ok": true, "projectKey": projectKey, "builds": out})
		},
	}
}

func (a *AIService) toolClusterResources() aiTool {
	return aiTool{
		Name: "get_cluster_resources",
		Description: "查某个环境所在集群的**实时**资源：节点数、K8s 版本、可分配 CPU / 内存、当前使用率。" +
			"注意返回里的 metricsAvailable=false 表示**没读到用量指标**（集群没装 metrics-server 或凭据无权读），" +
			"此时用量为 0 是「没读到」而不是「用得少」，不要把它说成资源空闲。",
		Parameters: schema(map[string]any{
			"envKey": map[string]any{"type": "string", "description": "环境的 env_key"},
		}, []string{"envKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey := argStr(args, "envKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" {
				return "", errors.New("缺少 envKey")
			}
			env, err := a.aiEnvByKey(ctx, envKey)
			if err != nil {
				return jsonText(failResult(err))
			}
			st, err := a.deps.Up.Cluster.Nodes(ctx, clusterRefOf(env))
			if err != nil {
				return jsonText(failResult(fmt.Errorf("读集群资源失败：%w", err)))
			}
			return jsonText(map[string]any{
				"ok": true, "envKey": envKey, "cluster": env.Cluster,
				"nodes": st.Nodes, "version": st.Version,
				"metricsAvailable": st.MetricsAvailable,
				"cpuCores":         st.CPUCores,
				"memBytes":         st.MemBytes,
				"cpuUsagePercent":  st.CPUUsage,
				"memUsagePercent":  st.MemUsage,
			})
		},
	}
}

// --------------------------------------------------------------------------- 执行

// AIToolResult 一次工具执行的留痕。
//
// 导出是因为它要经 SSE 的 tool 事件推给前端做「正在查询集群…」展示 ——
// 那段等待期里没有 token，不给个进度用户只会以为卡死了。
// Result 原样带上（而非只给摘要）：出了问题要能一眼看出"模型当时看到的到底是什么"。
type AIToolResult struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Args   string `json:"args"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

// callTool 执行一次工具调用。
//
// **刻意不向上抛错**：工具失败要作为结果回灌给模型（让它如实转述"这次没查到"），
// 而不是让整条对话崩掉。返回的 error 只用于"工具名根本不存在"这种编程错误 ——
// 那也必须回灌，否则模型会一直重试同一个不存在的函数。
func (a *AIService) callTool(ctx context.Context, name, rawArgs string) AIToolResult {
	res := AIToolResult{Name: name, Args: rawArgs}
	tool := a.findTool(name)
	if tool == nil {
		res.Error = "没有名为 " + name + " 的工具"
		res.Result = aiJSON(map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("不存在名为 %q 的工具。可用工具：%s。", name, a.aiToolNames()),
		})
		return res
	}
	res.Label = aiToolLabel(name)

	args := map[string]any{}
	if trimmed := strings.TrimSpace(rawArgs); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			// 参数不合法是模型的锅，但也是**它自己要处理**的情况：回灌原文让它改。
			res.Error = "参数不是合法 JSON"
			res.Result = aiJSON(map[string]any{
				"ok":    false,
				"error": "参数不是合法 JSON，请重新发起调用并给出合法的 JSON 对象。原文：" + trimmed,
			})
			return res
		}
	}

	out, err := tool.Run(ctx, CallerFrom(ctx), args)
	if err != nil {
		res.Error = err.Error()
		res.Result = aiJSON(map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("工具 %s 执行失败：%v", name, err),
		})
		return res
	}
	res.Result = out
	return res
}

// aiToolNames 供错误提示列出可用工具。
func (a *AIService) aiToolNames() string {
	all := a.aiTools()
	names := make([]string, 0, len(all))
	for _, t := range all {
		names = append(names, t.Name)
	}
	return strings.Join(names, "、")
}

// aiToolLabel 给前端看的"正在做什么"，用人话而不是函数名。
func aiToolLabel(name string) string {
	switch name {
	case "list_environments":
		return "读取环境名录"
	case "list_projects":
		return "读取项目名录"
	case "list_env_services":
		return "读取该环境的服务清单"
	case "get_service_status":
		return "查询集群实时状态"
	case "diagnose_service":
		return "采集集群侧故障证据"
	case "list_deployments":
		return "读取部署记录"
	case "list_builds":
		return "读取构建记录"
	case "get_cluster_resources":
		return "读取集群资源"
	}
	return "调用工具 " + name
}

// --------------------------------------------------------------------------- 小工具

// aiEnvByKey 按 env_key 取环境。
func (a *AIService) aiEnvByKey(ctx context.Context, key string) (*model.Environment, error) {
	var e model.Environment
	if err := a.db(ctx).Where("env_key = ?", key).First(&e).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("平台里没有 env_key=%q 的环境，请先用 list_environments 确认标识", key)
		}
		return nil, err
	}
	return &e, nil
}

// aiWorkloadOf 解析「要查哪个工作负载」，并说明它是否已纳管。
//
// 为什么要允许**未纳管也查**：所有集群读取原本都以 service_instance 为入口
// （要先纳管才知道 workload_name），于是"集群里有没有某个未纳管的工作负载"
// 这个问题，平台自己都答不了 —— 而它恰恰是有价值的运维信号：
// 集群里存在、但平台不跟踪 = 有人手工部署过东西。
//
// 实测（2026-10-27，155 上线库）：51 条服务实例的 workload_name **全部等于**
// project_key，无一例外。所以未纳管时退回 projectKey 不是猜测，
// 而是平台自己的命名约定；查的是一个**按名字 Get** 的资源，不是 List，不需要额外权限。
//
// 注意：只有"没有纳管记录"才回退。数据库真出错要如实报错 ——
// 静默退回 projectKey 会去查一个可能根本不该查的名字，比报错更难查。
func (a *AIService) aiWorkloadOf(ctx context.Context, envKey, projectKey string) (string, bool, error) {
	var s model.ServiceInstance
	err := a.db(ctx).Where("env_key = ? AND project_key = ?", envKey, projectKey).First(&s).Error
	if err == nil {
		// 纳管记录存在但 workload_name 为空（历史数据）时同样退回 projectKey。
		if n := strings.TrimSpace(s.WorkloadName); n != "" {
			return n, true, nil
		}
		return projectKey, true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", false, err
	}
	if strings.TrimSpace(projectKey) == "" {
		return "", false, errors.New("缺少 projectKey")
	}
	return projectKey, false, nil
}

// aiServiceInstance 取服务实例（含集群里的工作负载名，工具查集群要用）。
func (a *AIService) aiServiceInstance(ctx context.Context, envKey, projectKey string) (*model.ServiceInstance, error) {
	var s model.ServiceInstance
	err := a.db(ctx).Where("env_key = ? AND project_key = ?", envKey, projectKey).First(&s).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("环境 %s 里没有 %s 的服务实例（可能尚未部署过，或标识不对）", envKey, projectKey)
		}
		return nil, err
	}
	return &s, nil
}

// lastDeployOf 该服务在该环境最近一次部署单。
func (a *AIService) lastDeployOf(ctx context.Context, envKey, projectKey string) (map[string]any, bool) {
	var d model.DeployRecord
	err := a.db(ctx).Where("env_key = ? AND project_key = ?", envKey, projectKey).
		Order("id desc").First(&d).Error
	if err != nil {
		return nil, false
	}
	return map[string]any{
		"deployNo": d.DeployNo, "tag": tagOf(d.Image), "status": d.Status,
		"operator": d.OperatorName, "createdAt": formatTime(d.CreatedAt),
	}, true
}

// failResult 统一的失败回灌形态。
//
// 单独一个 constructor 是为了让「ok=false + error」这条约定只有一处，
// 不至于某个工具忘了带 ok 字段、让模型把失败读成成功。
func failResult(err error) map[string]any {
	return map[string]any{"ok": false, "error": err.Error()}
}

// jsonText 把结果序列化成给模型看的文本。
func jsonText(v any) (string, error) { return aiJSON(v), nil }

// aiJSON 序列化。失败时退回一句可读的说明而不是空串 ——
//
// 不复用同包里那个返回 model.JSONText 的 mustJSON：那个是给"要落库的 JSON 列"用的，
// 序列化失败时退回 `[]`（对数组列是对的，对这里会把失败伪装成"空结果"）。
// 空串会被模型当成"查到了，但没有内容"。
func aiJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"ok":false,"error":"结果序列化失败"}`
	}
	return string(b)
}

// tailLines 取文本末尾 n 行。
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// schema 拼 JSON Schema；required 为空时不写该字段。
func schema(props map[string]any, required []string) map[string]any {
	// 空对象要序列化成 {} 而不是 null：有的网关对 tools[].function.parameters
	// 为 null 直接判 400。
	if props == nil {
		props = map[string]any{}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// argStr 取字符串参数。
func argStr(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		// 模型偶尔把标识写成数字（如年份式标识），转成字符串比直接丢弃好。
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

// argInt 取整数参数并夹到 [min,max]。
//
// 模型给的数字可能是 float64（JSON 数字）也可能是字符串（"10"），
// 两种都要认，否则会静默退回默认值 —— 用户看到的就是"我明明说了 20 条却只给 5 条"。
func argInt(args map[string]any, key string, def, min, max int) int {
	v, ok := args[key]
	if !ok || v == nil {
		return def
	}
	n := def
	switch t := v.(type) {
	case float64:
		n = int(t)
	case string:
		if p, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			n = p
		}
	}
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}
