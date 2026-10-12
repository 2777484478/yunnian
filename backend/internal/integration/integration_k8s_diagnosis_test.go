package integration

import (
	"strings"
	"testing"
)

// javaFrameLine 照抄实测日志里 `at ...` 帧行的形状（带 jar 坐标，约 175 字节）。
//
// 长度是重点：Java 崩溃日志的体积几乎全堆在这些帧行上，而真正的答案
// （最后那条 `Caused by:`）只占总字节的零头。所以"按字节截断"这件事
// 一旦方向搞反，丢掉的恰好是信息密度最高的那一行。
const javaFrameLine = "\tat org.springframework.beans.factory.support.DefaultListableBeanFactory.doResolveDependency(DefaultListableBeanFactory.java:1307) ~[spring-beans-5.2.8.RELEASE.jar!/:5.2.8.RELEASE]\n"

// javaRootCauseLine 是实测 demobasic 崩溃日志里最关键的那一行原文。
const javaRootCauseLine = "Caused by: java.lang.IllegalArgumentException: Could not resolve placeholder 'messagelibrary.mq.username' in value \"${messagelibrary.mq.username}\"\n"

// javaCrashLog 造一份 frames 行帧 + 1 行根因的日志，形状与实测一致。
func javaCrashLog(frames int) string {
	var b strings.Builder
	for i := 0; i < frames; i++ {
		b.WriteString(javaFrameLine)
	}
	b.WriteString(javaRootCauseLine)
	return b.String()
}

// TestTailLogText_KeepsTailNotHead 是本轮真实事故的回归测试。
//
// 旧实现用 `io.LimitReader` 从**流的开头**截字节，语义是"保留最早的 N 字节"——
// 而崩溃的根因在最后一行。失败是静默的：字节数"达标"、接口不报错，
// 只是答案没了；模型于是拿着满屏 `at org.springframework...` 帧行，
// 编出一个"应用进程在端口 9629 上未监听"这类**看似合理**的结论。
// 方向搞反必须被这个测试挡住。
func TestTailLogText_KeepsTailNotHead(t *testing.T) {
	full := javaCrashLog(400) // 远大于 evidenceLogBytes，逼出截断
	if len(full) <= evidenceLogBytes {
		t.Fatalf("fixture 只有 %d 字节，不足以触发截断（上限 %d）", len(full), evidenceLogBytes)
	}

	got := tailLogText(full, evidenceLogBytes)

	if !strings.Contains(got, javaRootCauseLine) {
		t.Fatal("截断后丢了根因行 —— 说明又在按头部截断")
	}
	if !strings.Contains(got, "at org.springframework") {
		t.Error("连一行帧上下文都没留：模型会看不出这是一条 Java 异常链")
	}
	if len(got) > evidenceLogBytes {
		t.Errorf("保留 %d 字节，超过上限 %d", len(got), evidenceLogBytes)
	}
}

// TestTailLogText_KeepsWholeTextWhenItFits 未超限时必须原样返回。
//
// 这个用例防的是"修过头"：为了保尾部而顺手在末尾加省略号 / 反转行序之类的改动，
// 会让提示词里的人读部分与真实日志不一致，反而更难对照原始日志排查。
func TestTailLogText_KeepsWholeTextWhenItFits(t *testing.T) {
	in := "line-1\nline-2\n"
	if got := tailLogText(in, evidenceLogBytes); got != in {
		t.Errorf("未超限的日志被改动了：%q", got)
	}
}

// TestEvidenceLogLimits_CoverARealJavaCrash 钉住两个常量的**配合关系**，
// 而不只是各自的值：按行数上限取回来的东西，必须装得进字节上限。
//
// 否则就会出现"悄悄少几行"，而少的那几行恰好是根因 —— 这正是线上那次
// `ROLLOUT_STUCK` 诊断给出错误结论的直接原因（第 47 行 8437 字节，8KB 边界在 44 行）。
func TestEvidenceLogLimits_CoverARealJavaCrash(t *testing.T) {
	log := javaCrashLog(evidenceLogLines - 1) // 79 行帧 + 1 行根因 = 满额
	if len(log) <= 8<<10 {
		t.Fatalf("fixture 只有 %d 字节，证明不了旧上限（8KB）会出问题", len(log))
	}
	if got := tailLogText(log, evidenceLogBytes); got != log {
		t.Errorf("满额的真实形状日志被截断了（%d → %d 字节）：字节上限没有余量，"+
			"行数上限形同虚设", len(log), len(got))
	}
}

// TestTailLogText_AlignsToLineBoundary 截断处不能留半行，也不能留 U+FFFD。
//
// 两者都会污染证据：半行读不通，替换字符会让模型以为"编码坏了"而整块跳过。
func TestTailLogText_AlignsToLineBoundary(t *testing.T) {
	full := strings.Repeat("中文日志行，每行都含多字节字符\n", 3000)
	if len(full) <= evidenceLogBytes {
		t.Fatalf("fixture 只有 %d 字节，不足以触发截断", len(full))
	}

	got := tailLogText(full, evidenceLogBytes)

	if strings.ContainsRune(got, '\uFFFD') {
		t.Error("截断处留下了替换字符（U+FFFD）")
	}
	if !strings.HasPrefix(got, "中") {
		t.Errorf("第一行是半截的，没有对齐到行边界：%q", firstLine(got))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
