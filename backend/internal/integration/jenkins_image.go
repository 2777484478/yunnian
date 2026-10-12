package integration

import (
	"regexp"
	"strings"
)

// 从 Jenkins 构建日志里认出"这次构建到底推了哪个镜像版本"。
//
// 为什么只能这么做：Jenkins 的 Job 是使用方自己写的自由脚本，平台不参与它的构建过程，
// 所以**没有任何接口**能直接问出"这次产出的镜像版本是多少"。唯一可靠的事实来源就是
// 它自己打出来的日志——打包脚本里那句 `docker push` 就是最终产物：
//
//	++ cat /root/images/demowarning/ver.config
//	+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1058
//	The push refers to a repository [es-repo.example.com:5000/escloud/demowarning]
//
// 注意命令前那个 `+ `：脚本里开了 `set -x`，所以整条命令是回显出来的，
// 解析不能按行首匹配，也不能假设 "docker push" 一定出现在行首。
//
// 另一个要点：**只认带显式 tag 的引用**。`docker push repo`（不带 tag）推的是 latest，
// 那不是一个"版本号"；把它登记成版本只会让人以为构建产出了 latest。
// 同理，变量没展开（`$TAG`）的写法也不能当事实——那是脚本原文，不是结果。
var jenkinsDockerPush = regexp.MustCompile(`docker\s+push\b([^\n]*)`)

// ParsePushedImage 从日志文本里得出这次构建推送的镜像引用与版本号；取不到返回空串。
//
// wantRepo 是项目自己的仓库名（如 `demowarning`，可带路径前缀）。一个 Job 在中途
// 可能还会推别的镜像（编译缓存、基础镜像、旁路的辅助镜像），所以**优先**取与项目仓库
// 同名的那个；没有同名的才退回最后一个候选。
// 两者都取不到就返回空——宁可留空，也不要登记一个不属于这个项目的镜像，
// 那种错看起来完全合理，几乎不会被发现。
func ParsePushedImage(logText, wantRepo string) (ref string, tag string) {
	cands := pushedImageCandidates(logText)
	if len(cands) == 0 {
		return "", ""
	}

	want := imageRepoName(wantRepo)
	picked := ""
	if want != "" {
		for _, c := range cands {
			if imageRepoName(c) == want {
				// 继续往后找：同名的可能推了不止一次（多架构、或先推快照再推正式），
				// 日志里最后一次才是这次的最终产物。
				picked = c
			}
		}
	}
	if picked == "" {
		picked = cands[len(cands)-1]
	}
	t, _ := pushedImageTag(picked)
	return picked, t
}

// pushedImageCandidates 按出现顺序抽出所有"像产物"的镜像引用。
func pushedImageCandidates(logText string) []string {
	var out []string
	for _, m := range jenkinsDockerPush.FindAllStringSubmatch(logText, -1) {
		for _, tok := range strings.Fields(m[1]) {
			// `docker push --quiet repo:tag` 这类写法：跳过选项，取第一个非选项参数。
			if strings.HasPrefix(tok, "-") {
				continue
			}
			ref := strings.Trim(tok, `"'`+"`"+`;,&()`)
			if _, ok := pushedImageTag(ref); ok {
				out = append(out, ref)
			}
			break
		}
	}
	return out
}

// pushedImageTag 判断一个 token 是不是"带显式 tag 的镜像引用"，是则返回 tag。
func pushedImageTag(ref string) (string, bool) {
	if ref == "" || strings.ContainsAny(ref, "$`<>") {
		// 含 `$` 说明脚本里是变量、日志里没展开成实际值；`<>` 是占位写法。
		return "", false
	}
	if strings.Contains(ref, "@") {
		// 按 digest 推送：没有 tag，也不是版本号。
		return "", false
	}
	colon := strings.LastIndex(ref, ":")
	slash := strings.LastIndex(ref, "/")
	if colon <= slash {
		// 冒号出现在最后一个斜杠之前，说明它是 registry 的端口（host:5000/repo），
		// 而不是 tag。这种情况推的是 latest，不算版本号。
		return "", false
	}
	tag := ref[colon+1:]
	if tag == "" || strings.ContainsAny(tag, " /:") {
		return "", false
	}
	return tag, true
}

// imageRepoName 取镜像引用的仓库名（最后一段路径），忽略大小写与 tag。
func imageRepoName(ref string) string {
	ref = strings.Trim(strings.TrimSpace(ref), "/")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = ref[i+1:]
	}
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		ref = ref[:i]
	}
	return strings.ToLower(ref)
}
