// Package buildscript 提供「自建构建」的默认定义：默认构建脚本与默认 Tag 规则。
//
// 单独一个包而不是塞进 service：种子数据（store）也要给 script 模式的项目配一份
// 默认脚本，而 store 不能 import service（service → store，会成环）。
// 让两边都依赖这个没有下游的叶子包，比让种子项目留一个空脚本要好 ——
// 空脚本在构建时会安静地跑完然后什么都没产出。
package buildscript

import (
	_ "embed"
	"strings"
)

// 默认构建脚本。
//
// 用 go:embed 把脚本原样编进二进制，而不是写成 Go 字符串常量：
// 它们是**能直接执行的 shell**，放在 .sh 文件里有语法高亮、能被 shellcheck、
// 能单独跑起来验一遍；塞进反引号字符串则这三种能力全丢，而且一不小心还会
// 把 `$` 和反引号解释掉 —— 那是"改了一行默认脚本、构建机上报 command not found"
// 这类问题的经典来源。
//
//go:embed templates/build-backend.sh
var backendScript string

//go:embed templates/build-frontend.sh
var frontendScript string

// DefaultScript 按项目类型返回默认构建脚本。
//
// 只分前端/后端两档，不按语言再细分：脚本正文**在界面上可编辑**，
// 它提供的是"能跑通的最小完整例子"，不是穷举所有技术栈的模板库。
// 分得越细，用户越要先在一堆相似的模板里找到自己那份 —— 那比直接改两行更费事。
//
// 返回的文本**末尾带一个换行**：用户拿到编辑器里改的时候，光标不该停在最后一行
// 的行尾把最后一个字符顶掉。
func DefaultScript(projectType string) string {
	if strings.TrimSpace(projectType) == "frontend" {
		return strings.TrimSpace(frontendScript) + "\n"
	}
	return strings.TrimSpace(backendScript) + "\n"
}

// DefaultTagPattern 默认的镜像 Tag 规则。
//
// `{buildNo}` 是**项目级构建序号，每构建一次 +1**，所以默认出来的 tag 就是
// v1、v2、v3… —— 每次构建自动递增一个版本号，且天然唯一
// （不像时间戳那版，一眼看不出哪个更新）。
//
// 想留语义化前缀就写 `v1.0.{buildNo}`；多分支各自递增用 `{branch}-{buildNo}`。
const DefaultTagPattern = "v{buildNo}"

// DefaultScriptTagPattern 「自定义脚本」项目的默认 Tag 规则。
//
// 与 dockerfile / jenkins 用 `v{buildNo}`（第 1 次就是 v1）不同，脚本项目多半是
// 自建流水线接管过来的，沿用"内部版本号连续递增"的约定，例如 2.2.0.1000、2.2.0.1001…
// 而 `{buildNo}` 是**从 1 开始的构建序号**，所以这里用内联偏移 `{buildNo+999}` 表达
// 「第 1 次构建 = 1000，之后每构建一次 +1」。
//
// ⚠️ 序号是"第几次构建"、不是"第几次成功"：失败也会占掉一个号（见 renderTag 的说明），
// 所以 tag 序列里可能出现空洞（1000、1001、1003）。这是刻意的 —— 换成"只数成功次数"
// 会让同一 tag 被下一个成功构建覆盖，版本号不再唯一。
const DefaultScriptTagPattern = "2.2.0.{buildNo+999}"
