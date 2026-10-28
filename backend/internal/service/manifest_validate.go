package service

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/2777484478/yunnian/internal/errs"
)

// --------------------------------------------------------------------------- k8s 结构校验
//
// 为什么必须做：`parseManifest` 只查"是不是合法 YAML、有没有 kind/name"。
// 一份这样的文件它能顺利收下：
//
//	spec:
//	  replicas: "两个"          # 类型错
//	  template:
//	    spec:
//	      containers:
//	        - name: c
//	          image: x
//	          port: 80          # 字段名拼错，应该是 ports
//
// 而这些东西要到 apply 那一刻才炸，且那时平台只会说"应用失败"，
// 用户得回头翻四五十行 YAML 找哪一行有问题。
//
// 校验手段用的是 **k8s 自己的类型定义**（`client-go` 的 scheme + 严格解码），
// 不是平台自己臆想的一套规则：
//   - 严格解码（Strict）把**未知字段**当错误 —— 这正是"字段名拼错"的检出方式
//   - 类型不对会被 Go 的 json 解码直接拒绝
//   - 校验用到的 apiVersion/字段集合与 client-go 版本一致（当前 v0.31.5）

// k8sStrictDecoder 严格 YAML 解码器。
//
// 复用一份即可：serializer 内部无状态，并发安全。
// `Strict: true` 是关键 —— 默认的非严格模式会**静默忽略**认不出的字段，
// 那样拼错的 `port: 80` 会被无声丢弃，比报错更糟（用户以为生效了）。
var k8sStrictDecoder = json.NewSerializerWithOptions(
	json.DefaultMetaFactory, scheme.Scheme, scheme.Scheme,
	json.SerializerOptions{Yaml: true, Strict: true},
)

// errsInvalidWrap 让校验错误统一走「参数错误」这一档（HTTP 400），
// 与其它保存校验一致，前端不需要为它单独分支。
func invalidf(format string, args ...any) error {
	return errs.Invalid(fmt.Sprintf(format, args...))
}

// validateManifestStructure 用 k8s 的类型定义校验一份单资源 YAML。
//
// 返回值 warn 非空表示"校验没能覆盖它"（目前只有 CRD 一种），
// 调用方可以选择把它记进备注或原样存下 —— 但不能因此拒收：
// 自定义资源本来就是平台的合法内容，平台只是不认识它的字段。
func validateManifestStructure(content string) (warn string, err error) {
	_, _, derr := k8sStrictDecoder.Decode([]byte(content), nil, nil)
	if derr != nil {
		if unknownKindRe.MatchString(derr.Error()) {
			return "这是平台内置类型之外的自定义资源（CRD）：只校验了 YAML 语法，" +
				"它的字段结构平台不认识，无法校验", nil
		}
		return "", invalidf("%s", humanizeDecodeError(derr))
	}
	// 解码成功说明字段名和类型都对得上；但 k8s 的 Go 结构体区分不了
	// "字段没写" 和 "字段写了零值"，必填项得自己再查一遍。
	if err := checkRequiredFields(content); err != nil {
		return "", err
	}
	return "", nil
}

var (
	unknownKindRe  = regexp.MustCompile(`no kind "([^"]+)" is registered`)
	unknownFieldRe = regexp.MustCompile(`unknown field "([^"]+)"`)
	unmarshalRe    = regexp.MustCompile(`cannot unmarshal (\S+) into Go struct field (\S+) of type (\S+)`)
	yamlSyntaxRe   = regexp.MustCompile(`yaml: line (\d+): (.*)`)
)

// humanizeDecodeError 把 k8s 的英文解码报错翻成"能照着改"的中文。
//
// 不翻译的话用户看到的是：
//
//	strict decoding error: unknown field "spec.template.spec.containers[0].port"
//
// 这句话其实信息很足（路径精确到数组下标），但「unknown field」
// 对不熟 k8s 的人是黑话；直接丢了又可惜，所以保留原路径、换成人话前缀。
func humanizeDecodeError(err error) string {
	msg := err.Error()

	// ① 未知字段（可能多个，k8s 会一次报全）
	if fields := unknownFieldRe.FindAllStringSubmatch(msg, -1); len(fields) > 0 {
		paths := make([]string, 0, len(fields))
		for _, f := range fields {
			paths = append(paths, f[1])
		}
		return fmt.Sprintf("字段名不对：%s 不存在（检查是不是拼错了，或 k8s 版本不支持这个字段）",
			strings.Join(paths, "、"))
	}

	// ② 类型不匹配
	if m := unmarshalRe.FindStringSubmatch(msg); len(m) == 4 {
		return fmt.Sprintf("%s 的类型不对：写成了 %s，这里需要 %s", cleanFieldPath(m[2]), m[1], m[3])
	}

	// ③ YAML 语法（缩进、冒号后缺空格之类）
	if m := yamlSyntaxRe.FindStringSubmatch(msg); len(m) == 3 {
		return fmt.Sprintf("YAML 第 %s 行语法错误：%s", m[1], m[2])
	}

	// ④ 其它：保留原文，但去掉对用户无意义的 "strict decoding error: " 前缀
	clean := strings.TrimPrefix(msg, "strict decoding error: ")
	return "结构校验没通过：" + clean
}

// yamlHints 把 yaml.v3 的英文报错映射成"知道该改哪儿"的中文。
//
// 为什么要逐条翻译：yaml 的报错原文只给行号 + 一句英文（`did not find expected key`），
// 而这几句话对不熟 YAML 的人是黑话 —— 他只知道"第 4 行错了"，
// 不知道是缩进对不齐、还是冒号后少了空格、还是混进了 Tab。
var yamlHints = []struct{ pat, hint string }{
	{"did not find expected key",
		"缩进对不齐：这一行的缩进和上文不在同一层（YAML 只认空格，且同级必须严格对齐）"},
	{"mapping values are not allowed in this context",
		"「键: 值」的位置不对：最常见是冒号后面少了个空格，或这一行缩进多/少了一层"},
	{"could not find expected ':'",
		"少了一个冒号：这一行看起来像「键 值」，YAML 要求写成「键: 值」"},
	{"found character that cannot start any token",
		"这一行出现了不能作为行首的字符：最常见是用了 Tab（YAML 只允许空格缩进）"},
	{"did not find expected '-' indicator",
		"列表项的「-」缺失或缩进不对：列表每个元素的行首要有一个「- 」"},
	{"found unexpected end of stream",
		"内容提前结束了：后面可能有引号或括号没闭合"},
	{"did not find expected <document start>",
		"这里应该开始一个新文档：「---」后面不能再直接跟内容以外的东西"},
}

// humanizeYAMLError YAML 语法错误 → 中文（带行号）。
func humanizeYAMLError(err error) string {
	msg := err.Error()
	m := yamlSyntaxRe.FindStringSubmatch(msg)
	if len(m) != 3 {
		return msg
	}
	line, rest := m[1], m[2]
	for _, h := range yamlHints {
		if strings.Contains(rest, h.pat) {
			return fmt.Sprintf("第 %s 行 —— %s", line, h.hint)
		}
	}
	// 没命中映射表：至少把行号留下，英文原文附后
	return fmt.Sprintf("第 %s 行 —— %s", line, rest)
}

// cleanFieldPath 把 `DeploymentSpec.spec.replicas` 这种 Go 结构体名去掉，只留字段路径。
//
// `DeploymentSpec.spec.replicas` 里的结构体名是 Go 内部实现，对用户是噪音；
// 真正有用的是 `spec.replicas`。
func cleanFieldPath(s string) string {
	if i := strings.Index(s, ".spec"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// workloadKinds 有 Pod 模板、且 k8s 硬性要求 selector 的工作负载。
//
// 为什么只查这几类：`spec.selector` 缺失时 api server 一定拒绝
// （"spec.selector: Required value"），这是**不会误判**的硬规则。
// 其它字段（比如 Service 的 ports）有 ExternalName 这类例外，
// 查了反而可能拒掉合法内容，所以不查。
var workloadKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
	"ReplicaSet":  true,
}

// podTemplateKinds 有 Pod 模板、但不需要 selector 的。
var podTemplateKinds = map[string]bool{
	"Job":     true,
	"CronJob": true,
	"Pod":     true,
}

// checkRequiredFields 补上"Go 结构体区分不了未设置"的那部分必填校验。
//
// 只查两类，都是 apply 时 100% 会失败、且没有合理例外的：
//  1. Deployment / StatefulSet / DaemonSet / ReplicaSet 必须有 spec.selector
//  2. 所有带 Pod 模板的资源至少要有一个容器，且每个容器都有 image ——
//     没有 image 的容器 k8s 会拒（"spec.containers[0].image: Required value"）
func checkRequiredFields(content string) error {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		// 走到这里说明严格解码已经过了，理论上不会失败；
		// 真失败就交给 parseManifest 那层去报，这里不重复报错。
		return nil
	}
	kind, _ := doc["kind"].(string)
	kind = strings.TrimSpace(kind)

	if workloadKinds[kind] {
		spec, _ := doc["spec"].(map[string]any)
		if sel, ok := spec["selector"]; !ok || sel == nil {
			return invalidf("缺少 spec.selector：k8s 要求 %s 必须声明它来选择管理哪些 Pod，"+
				"通常写成\n\nspec:\n  selector:\n    matchLabels:\n      app: <你的应用名>", kind)
		}
	}

	if !workloadKinds[kind] && !podTemplateKinds[kind] {
		return nil
	}

	containers, path := podContainers(doc, kind)
	if path == "" {
		return nil
	}
	if len(containers) == 0 {
		return invalidf("%s 至少要有一个容器：缺少 %s", kind, path)
	}
	for i, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		img, _ := m["image"].(string)
		if strings.TrimSpace(img) == "" {
			name, _ := m["name"].(string)
			return invalidf("%s 里 %s[%d]（name=%s）没有 image：k8s 不会启动没有镜像的容器",
				kind, path, i, defaultValue(name, "未命名"))
		}
	}
	return nil
}

// podContainers 按 kind 取出容器列表和它的字段路径（路径用于报错定位）。
//
// CronJob 多一层 `jobTemplate.spec.template.spec`，单独处理 ——
// 否则会把"没有容器"误报成路径不存在。
func podContainers(doc map[string]any, kind string) ([]any, string) {
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		return nil, ""
	}
	var podSpec map[string]any
	var path string
	switch kind {
	case "Pod":
		podSpec = spec
		path = "spec.containers"
	case "CronJob":
		jt, _ := spec["jobTemplate"].(map[string]any)
		js, _ := jt["spec"].(map[string]any)
		tpl, _ := js["template"].(map[string]any)
		podSpec, _ = tpl["spec"].(map[string]any)
		path = "spec.jobTemplate.spec.template.spec.containers"
	default:
		tpl, _ := spec["template"].(map[string]any)
		podSpec, _ = tpl["spec"].(map[string]any)
		path = "spec.template.spec.containers"
	}
	if podSpec == nil {
		// 模板都还没写全，交给别的校验；这里不报"没有容器"以免误导
		return nil, ""
	}
	list, _ := podSpec["containers"].([]any)
	return list, path
}

func defaultValue(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
