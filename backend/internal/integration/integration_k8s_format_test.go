package integration

import (
	"testing"
	"time"
)

// humanAge 的单位拼接。重点是"第二段为 0 时不写出来"——
// 实测 Pod 列表里出现过 `128d0h`，夹在一屏 `26m` / `9s` 之间，
// 看着像是精度坏了，实际上是格式多写了一个 `0h`。
func TestHumanAge(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{9 * time.Second, "9s"},
		{59 * time.Second, "59s"},
		{26 * time.Minute, "26m"},
		{90 * time.Minute, "1h30m"},
		{time.Hour, "1h"},        // 不是 "1h0m"
		{5 * time.Hour, "5h"},
		{25 * time.Hour, "1d1h"},
		{48 * time.Hour, "2d"},   // 不是 "2d0h"
		{128 * 24 * time.Hour, "128d"},
		{-time.Minute, "0s"}, // 时钟漂移导致的负值按 0 处理，不显示 "-1m"
	}
	for _, c := range cases {
		if got := humanAge(c.in); got != c.want {
			t.Errorf("humanAge(%v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
