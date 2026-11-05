package service

import (
	"context"
	"errors"
)

// toolInspectEnvironment 环境巡检：一次采集全部健康信息，评分、渲染、存档。
//
// 为什么不复用现成的 list_env_services + get_cluster_resources 拼：
// 那样模型每问一次都要自己算"哪些不正常、内存算不算高"，结果是**同一个环境
// 两次问得到两种结论**。巡检的判读规则必须是确定的、可解释的（见 scoreInspection），
// 所以它落在代码里，模型只负责把结论讲成人话。
//
// 另一个差别：巡检会**存档**。用户隔一周回来问"上次巡检什么情况"，
// 只有存下来的报告能回答。
func (a *AIService) toolInspectEnvironment() aiTool {
	return aiTool{
		Name: "inspect_environment",
		Description: "对某个环境做**完整健康巡检**并生成一份报告（会存档、可下载）：" +
			"集群资源水位、服务就绪情况、TLS 证书到期时间、近期部署失败率，并给出 0-100 健康分与逐项结论。" +
			"用户说「帮我巡检一下 X 环境」「生成 X 的体检报告」「X 环境最近健康吗」时用它。" +
			"⚠️ 它一次就能拿到全部体检信息，**不要**再用 list_env_services / get_cluster_resources 逐个凑。" +
			"返回里 reportId 是存档编号。" +
			"⚠️ **不要在正文里拼下载链接或让用户手动访问某个路径** —— 前端会在工具卡片上" +
			"自动给出「查看报告」「下载 HTML」按钮，你只需说明报告编号、健康分与关键发现。" +
			"healthScore 的扣分依据都列在 findings 里（每条带 penalty），引用分数时请一并说明是哪些项扣的。",
		Parameters: schema(map[string]any{
			"envKey": map[string]any{"type": "string", "description": "环境的 env_key，先用 list_environments 确认"},
			"hours": map[string]any{"type": "integer",
				"description": "可选。统计部署活动的时间窗口（小时），默认 168（7 天），范围 1~720"},
		}, []string{"envKey"}),
		Run: func(ctx context.Context, c Caller, args map[string]any) (string, error) {
			envKey := argStr(args, "envKey")
			if err := assertToolEnvVisible(c, envKey); err != nil {
				return "", err
			}
			if envKey == "" {
				return "", errors.New("缺少 envKey")
			}
			if a.cd == nil {
				// 装配问题（New 里忘了传 cd）。如实报，不要退化成"巡检不了"。
				return jsonText(failResult(errors.New("巡检服务未装配，请联系管理员")))
			}
			res, err := a.cd.InspectEnvironment(ctx, InspectInput{
				EnvKey: envKey,
				Hours:  argInt(args, "hours", inspectionDefaultHours, 1, 720),
			}, c)
			if err != nil {
				return jsonText(failResult(err))
			}
			// **不回 HTML**：几十 KB 的标签文本对模型毫无价值，只会挤占上下文、
			// 还可能让它在正文里复述一堆样式名。它要的是结论与建议。
			return jsonText(map[string]any{
				"ok":           true,
				"envKey":       res.EnvKey,
				"envName":      res.EnvName,
				"healthScore":  res.Score,
				"level":        res.Level,
				"summary":      res.Summary,
				"stats":        res.Stats,
				"findings":     res.Findings,
				"reportId":     res.ReportID,
				"downloadUrl":  "/api/v1/inspection-reports/" + itoa64(res.ReportID) + "/download",
				"notes":        res.Notes,
				"archivedNote": "报告已存档为 #" + itoa64(res.ReportID) + "，可在「巡检报告」页查看彩色版并下载 HTML。",
			})
		},
	}
}

// itoa64 十进制的 uint64 转字符串。用 strconv 会更短，但这里只有一处用，
// 不值得为它加 import —— 保持文件依赖面最小。
func itoa64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
