package service

import "context"

/**
 * 把 Caller 放进 context。
 *
 * 为什么用 context 而不是给整条调用链加参数：
 * AI 工具的执行链路是 `Chat → runTool → callTool → tool.Run`，中间还夹着模型循环。
 * 为了给 8 个工具传一个"当前是谁在问"，把每个中间层都加一个参数，噪声比收益大；
 * 而 Caller 本来就是**这次请求**的属性 —— 它和 ctx 的生命周期完全一致，语义也合。
 *
 * ⚠️ 只放 Caller，不放别的业务状态：ctx 里的值应当是"请求级的横切信息"，
 * 不是传参的后门。取不到时返回零值 Caller（User=nil），可见性判断会按
 * "没登录 → 不限制"处理 —— 与现有各处 Caller 缺省时的行为一致。
 */
type callerCtxKey struct{}

// WithCaller 把 Caller 放进 ctx。
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerCtxKey{}, c)
}

// CallerFrom 从 ctx 取 Caller；没有则返回零值。
func CallerFrom(ctx context.Context) Caller {
	if v, ok := ctx.Value(callerCtxKey{}).(Caller); ok {
		return v
	}
	return Caller{}
}
