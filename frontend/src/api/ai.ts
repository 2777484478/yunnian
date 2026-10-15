import { http } from './client';
import { openSSE } from './sse';

// ---------------------------------------------------------------------------
// AI 能力（对应后端 handler_ai.go）
//
// 重要前提：AI 是**只读**的——它只能查询平台事实并给建议，不会执行任何变更。
// 助手回复里 kind='action' 的卡片只是建议，用户点击后走正常 REST（带权限校验与审计）。
// ---------------------------------------------------------------------------

export interface ChatAttachment {
  kind: 'table' | 'kv' | 'action';
  title: string;
  data: unknown;
}

export interface ChatInput {
  conversationId?: string;
  message: string;
  /** 附加上下文，如"针对构建 #123 分析失败原因" */
  context?: Record<string, unknown>;
}

/**
 * 一次工具调用的留痕。
 *
 * 为什么要透给前端：工具（尤其查集群的那两个）要几百毫秒到几秒，
 * 这段时间里**一个 token 都没有**，只显示"生成中"的用户体验就是"卡住了"。
 * 把"正在查询集群实时状态"这一步显示出来，等待才有内容。
 */
export interface ToolStep {
  name: string;
  /** 人话的步骤名，由后端给（前端不按函数名再映射一份，免得两边不一致） */
  label: string;
  args: string;
  /** 工具真实返回（JSON 文本）。原样带上，出问题能看清"模型当时看到了什么" */
  result: string;
  error?: string;
}

export interface ChatHandlers {
  /** 结构化卡片：一次回复里可能先于正文下发 */
  onAttachment?: (a: ChatAttachment) => void;
  /** 工具调用留痕：模型自己去查了一次数据 */
  onTool?: (t: ToolStep) => void;
  /** 增量文本。delta 是本次增量，full 是累积至今的完整文本（省得调用方自己拼） */
  onToken?: (delta: string, full: string) => void;
  onError?: (message: string) => void;
  /** 流结束。conversationId 由后端在 end 事件里给出 */
  onEnd?: (conversationId?: string) => void;
}

/**
 * 会话列表项。
 *
 * 2026-10-15 起会话**真落库**了（此前是空壳：一期不建表，后端返回空列表 +
 * usable=false）。原因见后端 `service/ai_conversation.go` 的文件头。
 */
export interface Conversation {
  /** 对外标识（conv_xxxxxx）。前端全程用它，不要试图解析 */
  key: string;
  /** 标题 = 首问的前若干字（后端生成），列表里靠它区分哪条是哪条 */
  title: string;
  /** 消息条数（user + assistant 合计） */
  msgCount: number;
  /**
   * 最后一条消息的时间（ISO8601 带时区）。
   * 左侧列表按它分「今天 / 7 天内 / 30 天内 / 更早」，所以不能用切片后的字符串，
   * 要交给 `new Date()` 解析。
   */
  lastMsgAt: string;
  createdAt: string;
}

export interface ConversationsResult {
  list: Conversation[];
  total: number;
}

/**
 * 会话里的一条历史消息。
 *
 * 字段与 SSE 事件同源（attachment / tool / token），所以回看时能把
 * "当时查了什么工具、拿到了什么卡片"原样重放，而不只是重放一段正文。
 */
export interface StoredMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  attachments?: ChatAttachment[];
  tools?: ToolStep[];
  at: string;
}

export interface ConversationDetail {
  conversation: Conversation;
  messages: StoredMessage[];
}

export const aiApi = {
  /**
   * 流式问答。返回的 Promise 在**整条流结束时**才 resolve，
   * 页面里不要 await 它来等"首个字"——用 handlers.onToken 驱动 UI。
   * 传 signal 可以在组件卸载/用户停止时中断。
   */
  chat(input: ChatInput, handlers: ChatHandlers, signal?: AbortSignal): Promise<void> {
    let full = '';
    return openSSE('/ai/chat', {
      method: 'POST',
      body: input,
      signal,
      onFrame: (f) => {
        let payload: any;
        try {
          payload = JSON.parse(f.data);
        } catch {
          return; // 非 JSON 帧直接忽略，不要让一条脏数据打断整条流
        }
        switch (f.event) {
          case 'attachment':
            handlers.onAttachment?.(payload as ChatAttachment);
            break;
          case 'tool':
            handlers.onTool?.(payload as ToolStep);
            break;
          case 'token': {
            const delta = String(payload?.delta ?? '');
            full += delta;
            handlers.onToken?.(delta, full);
            break;
          }
          case 'error':
            handlers.onError?.(String(payload?.message ?? 'AI 服务异常'));
            break;
          case 'end':
            handlers.onEnd?.(payload?.conversationId);
            break;
          default:
            break;
        }
      },
      onClose: (err) => {
        // 后端异常断开时 onEnd 不会来，这里兜底收尾，避免 UI 一直转圈
        if (err) handlers.onError?.(err.message);
        else handlers.onEnd?.();
      },
    });
  },

  /** 对失败构建做归因（非流式，一次返回） */
  analyzeBuild: (buildId: string) =>
    http.post<{ reason: string; suggestion: string; confidence: number }>(`/ai/analyze-build/${buildId}`),

  /** 对异常部署做归因 */
  analyzeDeploy: (deployId: string) =>
    http.post<{ reason: string; suggestion: string; confidence: number }>(`/ai/analyze-deploy/${deployId}`),

  /** 会话列表（只含**当前账号自己的**，后端按登录用户过滤）。 */
  conversations: (page = 1, pageSize = 50) =>
    http.get<ConversationsResult>('/ai/conversations', { page, pageSize }),

  /** 一个会话的全部消息（正序，从第一句开始）。 */
  conversation: (key: string) =>
    http.get<ConversationDetail>(`/ai/conversations/${encodeURIComponent(key)}`),

  /** 删除会话（消息一并清除）。 */
  deleteConversation: (key: string) =>
    http.del<{ deleted: boolean }>(`/ai/conversations/${encodeURIComponent(key)}`),
};
