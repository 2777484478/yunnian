import React from 'react';
import { Button, Result, Space, Typography } from 'antd';

const { Paragraph, Text } = Typography;

// ---------------------------------------------------------------------------
// 渲染错误兜底
//
// 为什么必须有这一层（2026-09-29 实测教训）：
// 集群接入页的后端把 `usedByEnvs` 这个列表字段的空值发成了 `null`，而前端按
// `string[]` 读、写了 `c.usedByEnvs.length` —— 一个字段读错，**整个 React 树被卸载**，
// 页面变成纯白，用户看到的是"平台挂了"，不是"这一页有个字段不对"。
// 那次白屏把侧边栏、顶栏一起带走了，用户连"点别处逃出去"都做不到。
//
// 所以这里做两件事：
//   1. 把崩溃限制在**局部**（页面内容区），侧边栏与顶栏仍然可用；
//   2. 把**真实错误**摊开给用户（消息 + 堆栈），而不是只说"出错了" ——
//      内部平台里，能复制一段堆栈比一句道歉有用得多。
//
// ⚠️ 能力边界（别指望它兜住一切）：React 的 error boundary **只捕获渲染与生命周期**
// 里抛出的错误。事件处理器（onClick 里 throw）、setTimeout、Promise 里抛的错误
// 都不会走到这里 —— 那些要靠调用方自己的 try/catch。
// 本组件解决的是"一个字段读错 → 整站白屏"这一类，也是实际发生过的那一类。
// ---------------------------------------------------------------------------

interface Props {
  children: React.ReactNode;
  /**
   * 变化时自动清空错误状态。
   *
   * 必要性：error boundary 的错误状态是粘住的——不重置的话，用户切到别的路由
   * 也还是那一屏报错，看起来像"整个平台坏了"。传当前路由进去，
   * 一切页就恢复正常，用户能自己走出去。
   */
  resetKey?: string;
  /** 出错时的简要说明，用来区分是哪个区域挂了 */
  scope?: string;
  /** 全局兜底（整个应用都渲染不出来）时用 fullscreen 版式 */
  fullscreen?: boolean;
}

interface State {
  error: Error | null;
  info: React.ErrorInfo | null;
}

class ErrorBoundary extends React.Component<Props, State> {
  state: State = { error: null, info: null };

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    // 同时打到控制台：用户截图 Network/Console 时能直接看到，不必先复现一遍
    console.error('[YunNian] 渲染异常被 ErrorBoundary 兜住', error, info);
    this.setState({ info });
  }

  componentDidUpdate(prev: Props) {
    if (prev.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: null, info: null });
    }
  }

  private reset = () => this.setState({ error: null, info: null });

  render() {
    const { error, info } = this.state;
    if (!error) return this.props.children;

    return (
      <Result
        status="error"
        title={this.props.scope ? `${this.props.scope}渲染出错` : '页面渲染出错'}
        subTitle="这一块已经被隔离，左侧导航与其它页面仍然可用。下面的错误信息可以直接复制反馈。"
        extra={
          <Space>
            <Button type="primary" onClick={this.reset}>
              重试
            </Button>
            <Button onClick={() => window.location.assign('#/overview')}>返回概览</Button>
          </Space>
        }
        style={this.props.fullscreen ? { paddingTop: 96 } : undefined}
      >
        <Paragraph style={{ marginBottom: 0 }}>
          <Text strong>{error.name}</Text>
          <Text style={{ marginLeft: 8 }}>{error.message || '（无消息）'}</Text>
        </Paragraph>
        {info?.componentStack || error.stack ? (
          <details style={{ marginTop: 8 }}>
            <summary style={{ cursor: 'pointer', color: '#1677ff', fontSize: 13 }}>
              展开技术细节（反馈问题时请一并附上）
            </summary>
            <pre
              style={{
                marginTop: 8,
                padding: 12,
                maxHeight: 280,
                overflow: 'auto',
                background: '#fafbfc',
                border: '1px solid #eceef2',
                borderRadius: 6,
                fontSize: 12,
                lineHeight: 1.6,
                whiteSpace: 'pre-wrap',
              }}
            >
              {error.stack ?? error.message}
              {info?.componentStack ? `\n\n---- 组件栈 ----${info.componentStack}` : ''}
            </pre>
          </details>
        ) : null}
      </Result>
    );
  }
}

export default ErrorBoundary;
