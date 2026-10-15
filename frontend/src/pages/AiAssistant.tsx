import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Alert, App as AntApp, Button, Input, Space, Table, Tag, Tooltip, Typography } from 'antd';
import {
  ApiOutlined,
  ClearOutlined,
  DeleteOutlined,
  LoadingOutlined,
  PlusOutlined,
  RobotOutlined,
  SendOutlined,
  SettingOutlined,
  StopOutlined,
  ThunderboltOutlined,
  UserOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { PageHeader } from '@/components/ui';
import { aiApi, environmentApi, modelApi } from '@/api';
import type { ChatAttachment, Conversation, StoredMessage, ToolStep } from '@/api';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { ago, fmt } from '@/utils/format';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// AI 运维助手
//
// 这一页有三处"不能糊弄"的地方，都是被真实接口定的：
//
// 1. **没有"切换模型"这回事**。后端 `ChatInput` 只有
//    {conversationId, message, context}——没有 model 字段；通道与模型由
//    `DefaultModelClient()` 按「平台默认通道」决定。所以这里**不做假下拉框**，
//    改为只读展示当前生效的通道，并指路到「模型设置」。
//
// 2. **错误不是 HTTP 错误**。实测：通道没有密钥时后端照样 200 开流，
//    然后下发 `event: error` + `event: end`。所以只 try/catch fetch 是不够的，
//    必须把 error 事件渲染到对话里，否则用户会看到"回答完了但一个字都没有"。
//
// 3. **AI 只读**。它不会真的执行部署/回滚；携带建议的卡片必须由用户
//    到对应页面手动发起（那里才有权限校验与审计）。所以卡片上的按钮
//    只做"跳转到该页面"，绝不显示"已执行"。
// ---------------------------------------------------------------------------

/**
 * 预置提问。
 *
 * 环境名从**真实数据**里取，不写死：写死就会出现"帮你看看 payment-service"
 * 这种提问，而平台里根本没有这个项目 —— 用户点了必然得到一个"查不到"的回答，
 * 看着就像 AI 不好使（实际是提问本身指向了不存在的东西）。
 *
 * 后两条刻意不带具体实体：它们考的是"会不会用工具去查"，任何数据下都答得上。
 */
function buildSuggestions(envs: { name: string }[]): string[] {
  const out: string[] = [];
  if (envs[0]) out.push(`${envs[0].name} 里有哪些服务不正常？`);
  if (envs[1]) out.push(`${envs[1].name} 最近一次部署成功了吗？`);
  out.push('最近 24 小时有哪些构建失败了？');
  out.push('某个服务一直起不来的话，怎么定位原因？');
  return out.slice(0, 4);
}

interface ChatMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  at: string;
  attachments?: ChatAttachment[];
  /** 这一轮里模型自己发起的查询（按发生顺序）。 */
  tools?: ToolStep[];
  /** 流式输出中 */
  streaming?: boolean;
  /** 后端下发的错误原文（可能是模型侧 401、超时等） */
  error?: string;
}

const now = () => fmt(new Date());

/**
 * 会话列表按时间分组（今天 / 7 天内 / 30 天内 / 更早）。
 *
 * 与 DeepSeek 同一套分法。列表一长，纯按时间倒序也认不出哪条是哪条；
 * 分组让人先按"大概什么时候聊的"定位，再扫标题。
 *
 * 空组不渲染（否则一个新用户会先看到四行光秃秃的"今天/7 天内/…"）。
 */
function groupConversations(list: Conversation[]): [string, Conversation[]][] {
  const d0 = new Date();
  const startOfToday = new Date(d0.getFullYear(), d0.getMonth(), d0.getDate()).getTime();
  const d7 = startOfToday - 6 * 86400_000;
  const d30 = startOfToday - 29 * 86400_000;
  const buckets: [string, Conversation[]][] = [
    ['今天', []],
    ['7 天内', []],
    ['30 天内', []],
    ['更早', []],
  ];
  for (const c of list) {
    const t = new Date(c.lastMsgAt).getTime();
    // 时间解析不出来时归到"更早"，**不要**丢进"今天"：
    // 那会让一条坏数据永远顶在最上面。
    if (Number.isNaN(t)) buckets[3][1].push(c);
    else if (t >= startOfToday) buckets[0][1].push(c);
    else if (t >= d7) buckets[1][1].push(c);
    else if (t >= d30) buckets[2][1].push(c);
    else buckets[3][1].push(c);
  }
  return buckets.filter(([, items]) => items.length > 0);
}

/**
 * 历史消息 → 页面消息模型。
 *
 * 只做字段搬运，不做加工：历史里的 attachments / tools 是**当时**的结构化留痕，
 * 原样交回给同一套渲染函数（renderTool / renderAttachment），
 * 这样"回看"和"刚发生"长得完全一样 —— 两套渲染迟早会漂移成两种说法。
 */
function toChatMessage(m: StoredMessage): ChatMessage {
  return {
    id: m.id,
    role: m.role,
    content: m.content,
    at: fmt(m.at),
    attachments: m.attachments,
    tools: m.tools,
  };
}

/**
 * 把工具入参/返回格式化成可读 JSON。
 *
 * 入参是模型生成的**字符串**（且可能不是合法 JSON），所以解不开就原样显示 ——
 * 那种情况下原文本身就是证据（"模型到底传了什么"）。
 */
function prettyJson(raw: string): string {
  const s = (raw ?? '').trim();
  if (!s) return '（无）';
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

/** 建议卡片 → 平台内页面的兜底映射（action 载荷形态后端未定，按关键词猜） */
function resolveRoute(action: string): string | null {
  const a = action.toLowerCase();
  if (a.includes('rollback') || a.includes('回滚')) return '/cd/rollback';
  if (a.includes('deploy') || a.includes('发布') || a.includes('部署')) return '/cd/topology';
  if (a.includes('build') || a.includes('构建')) return '/ci/builds';
  if (a.includes('env') || a.includes('环境')) return '/cd/envs';
  if (a.includes('model') || a.includes('模型')) return '/system/models';
  return null;
}

const AiAssistant: React.FC = () => {
  const { message, modal } = AntApp.useApp();
  const navigate = useNavigate();

  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [input, setInput] = useState('');
  const [streaming, setStreaming] = useState(false);
  const [conversationId, setConversationId] = useState<string | undefined>();
  /** 左栏会话列表（只含当前账号自己的，后端按登录用户过滤）。 */
  const [convs, setConvs] = useState<Conversation[]>([]);
  const [convsLoading, setConvsLoading] = useState(true);
  /** 正在打开的历史会话 key（用于把那一条显示成加载态）。 */
  const [openingKey, setOpeningKey] = useState<string | null>(null);
  const bodyRef = useRef<HTMLDivElement>(null);
  const abortRef = useRef<AbortController | null>(null);

  // 输入法组词状态。
  //
  // 必须自己追踪、不能只信 `event.nativeEvent.isComposing`：中文输入法在
  // **确认候选词**的那次回车序列里，compositionend 常常先于 keydown 触发，
  // 此时 isComposing 已经是 false，但那一下回车是"选字"不是"发送"。
  // 表现就是用户拼音还没打完，消息已经发出去了。
  //
  // compositionend 里延后一个 tick 再复位，正是为了盖住上面这个顺序。
  const composingRef = useRef(false);

  // 当前生效的通道：只读展示。后端按「平台默认通道」选，请求里改不了。
  //
  // 注意 /models 的权限点是 user:manage —— 普通运维账号读不到通道列表。
  // 这不是"平台没有可用模型"，而是"你无权查看模型配置"，两者要分开，
  // 否则用户会跑去让管理员加模型，而管理员一看明明配着好好的。
  const canSeeModels = hasPerm('user:manage');
  const modelsApi = useApi(() => modelApi.list(), [], { enabled: canSeeModels });
  const models = useMemo(() => modelsApi.data ?? [], [modelsApi.data]);
  const activeModel = models.find((m) => m.isDefault && m.enabled) ?? models.find((m) => m.isDefault);
  const usableModel = !!activeModel && activeModel.enabled;
  const keyMissing = !!activeModel && activeModel.apiKeyMasked === '未配置';
  /** 通道状态未知只可能是"没权限看"，因为请求本身没有失败（失败会走 error） */
  const modelUnknown = !canSeeModels;

  /**
   * 拉会话列表。
   *
   * 不用 `useApi`：那是"挂载时拉一次 + 手动 refresh"的形状，而这里需要在
   * **每次问答结束后**都刷一次 —— 新会话是后端在第一轮 Prepare 里创建的，
   * 不刷的话用户刚聊完却在左边找不到它（只有下次进页面才出现）。
   */
  const loadConversations = useCallback(async () => {
    try {
      const r = await aiApi.conversations(1, 100);
      setConvs(r.list);
    } catch (e) {
      // 列表拉不到不该影响提问（它只是个侧栏），但**必须说出来**：
      // 否则用户对着永远空着的左栏，会以为"历史功能没做"而反复重试。
      message.error(`会话列表加载失败：${(e as Error).message}`);
    } finally {
      setConvsLoading(false);
    }
  }, [message]);

  useEffect(() => {
    void loadConversations();
  }, [loadConversations]);

  // 只为预置提问取环境名。失败就不显示带环境名的那两条（下面有兜底），
  // **不**因此把整页打成错误态 —— 提问框本身跟这个列表没关系。
  const envsApi = useApi(() => environmentApi.list({ enabled: true, pageSize: 20 }), []);
  const suggestions = useMemo(
    () => buildSuggestions((envsApi.data?.list ?? []).map((e) => ({ name: e.name }))),
    [envsApi.data],
  );

  useEffect(() => {
    if (bodyRef.current) bodyRef.current.scrollTop = bodyRef.current.scrollHeight;
  }, [messages, streaming]);

  // 组件卸载时掐断流，避免离开页面后 SSE 还在跑
  useEffect(() => () => abortRef.current?.abort(), []);

  const stop = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    setStreaming(false);
    setMessages((m) => m.map((x) => (x.streaming ? { ...x, streaming: false } : x)));
  }, []);

  /** 开一段新对话：清空右侧，并**丢掉** conversationId。 */
  const newConversation = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    setStreaming(false);
    setMessages([]);
    setInput('');
    // 必须清掉旧 key：留着它的话，下一句会接着上一段对话的上下文继续问，
    // 而用户点的是「新建对话」。
    setConversationId(undefined);
  }, []);

  /**
   * 打开一条历史会话，把当时的消息原样铺回来。
   *
   * 工具留痕与结构化卡片一并还原 —— 用户翻记录多半就是想看
   * "当时到底查了哪几个服务、集群回了什么"。
   */
  const openConversation = useCallback(
    async (key: string) => {
      if (key === conversationId && messages.length > 0) return;
      if (streaming) {
        message.warning('正在回答中，请先点「停止」或等它结束');
        return;
      }
      setOpeningKey(key);
      try {
        const d = await aiApi.conversation(key);
        setConversationId(d.conversation.key);
        setMessages(d.messages.map(toChatMessage));
      } catch (e) {
        message.error(`打开会话失败：${(e as Error).message}`);
      } finally {
        setOpeningKey(null);
      }
    },
    [conversationId, message, messages.length, streaming],
  );

  /** 删除一条会话（二次确认：删掉找不回来）。 */
  const removeConversation = useCallback(
    (key: string, title: string) => {
      if (streaming) {
        message.warning('正在回答中，请先点「停止」再删除');
        return;
      }
      modal.confirm({
        title: `删除会话「${title}」？`,
        content: '这段对话的内容会一并清除，无法找回。',
        okText: '删除',
        okButtonProps: { danger: true },
        onOk: async () => {
          try {
            await aiApi.deleteConversation(key);
            message.success('已删除');
            // 删的正是当前打开的那条 → 顺手清空右侧，
            // 否则会出现"左边没了、右边还摊着"，下次提问又会带着已删会话的 key 去问。
            if (key === conversationId) newConversation();
            void loadConversations();
          } catch (e) {
            message.error(`删除失败：${(e as Error).message}`);
            throw e;
          }
        },
      });
    },
    [conversationId, loadConversations, message, modal, newConversation, streaming],
  );

  const ask = async (q: string) => {
    const question = q.trim();
    if (!question || streaming) return;

    const stamp = Date.now();
    const assistantId = `ai_${stamp}_r`;
    setInput('');
    setStreaming(true);
    setMessages((m) => [
      ...m,
      { id: `ai_${stamp}`, role: 'user', content: question, at: now() },
      { id: assistantId, role: 'assistant', content: '', at: now(), streaming: true },
    ]);

    const ac = new AbortController();
    abortRef.current = ac;

    const patch = (p: Partial<ChatMessage>) =>
      setMessages((m) => m.map((x) => (x.id === assistantId ? { ...x, ...p } : x)));

    await aiApi.chat(
      { conversationId, message: question },
      {
        onAttachment: (a) =>
          setMessages((m) =>
            m.map((x) => (x.id === assistantId ? { ...x, attachments: [...(x.attachments ?? []), a] } : x)),
          ),
        // 模型自己发起的一次查询。追加而不是替换：一轮回答里它可能查好几次
        // （先对标识、再查状态、发现异常后取证），全过程都要看得见。
        onTool: (t) =>
          setMessages((m) =>
            m.map((x) => (x.id === assistantId ? { ...x, tools: [...(x.tools ?? []), t] } : x)),
          ),
        onToken: (_delta, full) => patch({ content: full }),
        onError: (msg) => patch({ error: msg }),
        onEnd: (convId) => {
          if (convId) setConversationId(convId);
          patch({ streaming: false });
          setStreaming(false);
          abortRef.current = null;
        },
      },
      ac.signal,
    );

    // 无论正常结束还是被中断，都刷一次左栏。
    //
    // 会话在后端是**第一轮提问时**就建好的，用户点「停止」并不会撤销它 ——
    // 只信 onEnd 的话，中断这条路就不会刷新，用户会看到"我刚问过的话没进历史"。
    void loadConversations();
  };

  /**
   * 回车发送（带输入法守卫）。
   *
   * 不用 antd 的 onPressEnter：它内部只判 keyCode===13，组词中的回车照样触发。
   */
  const onInputKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key !== 'Enter') return;
    const native = e.nativeEvent as KeyboardEvent & { keyCode?: number };
    // 三道判据缺一不可：自维护的组词态、浏览器自报的组词态、以及老浏览器
    // 在组词期一律给 229 的约定。任一命中就当成"正在选字"，不发送。
    if (composingRef.current || native.isComposing || native.keyCode === 229) return;
    e.preventDefault();
    if (!streaming) ask(input);
  };

  /** 渲染一次工具调用。 */
  const renderTool = (t: ToolStep, key: number) => (
    <div className="yn-ai-tool" key={key}>
      <div className="yn-ai-tool__head">
        <span className="yn-ai-tool__dot" />
        <span>{t.label || t.name}</span>
        <span className="yn-mono" style={{ color: '#c2c7d0', fontSize: 11 }}>
          {t.name}
        </span>
        {t.error ? <span style={{ color: '#ff4d4f', fontSize: 11 }}>失败</span> : null}
      </div>
      <div className="yn-ai-tool__body">
        <div style={{ color: '#9aa0aa' }}>入参</div>
        <pre className="yn-mono">{prettyJson(t.args)}</pre>
        <div style={{ color: '#9aa0aa' }}>{t.error ? '失败原因' : '返回'}</div>
        <pre className="yn-mono">{prettyJson(t.error ? JSON.stringify({ error: t.error }) : t.result)}</pre>
      </div>
    </div>
  );

  const renderAttachment = (att: ChatAttachment, key: number) => {
    const data = att.data as any;
    return (
      <div className="yn-ai-attach" key={key}>
        <div className="yn-ai-attach__title">{att.title}</div>
        <div className="yn-ai-attach__body">
          {att.kind === 'kv' && data && typeof data === 'object' ? (
            Object.entries(data as Record<string, string>).map(([k, v]) => (
              <div className="yn-ai-kv-row" key={k}>
                <span>{k}</span>
                <span className="yn-mono">{String(v)}</span>
              </div>
            ))
          ) : null}

          {att.kind === 'table' && data?.columns ? (
            <Table
              size="small"
              pagination={false}
              rowKey={(_, i) => String(i)}
              dataSource={(data.rows as string[][]).map((r) =>
                Object.fromEntries(r.map((c, i) => [data.columns[i], c])),
              )}
              columns={data.columns.map((c: string) => ({ title: c, dataIndex: c, ellipsis: true }))}
              style={{ margin: '8px 0' }}
            />
          ) : null}

          {att.kind === 'action' && Array.isArray(data) ? (
            <>
              <Space size={8} wrap style={{ padding: '8px 0' }}>
                {(data as { label: string; action: string }[]).map((a) => {
                  const route = resolveRoute(a.action ?? a.label ?? '');
                  return (
                    <Button
                      key={a.label}
                      size="small"
                      onClick={() => {
                        if (route) {
                          navigate(route);
                        } else {
                          message.info(`请在平台对应页面手动执行「${a.label}」，AI 无变更权限`);
                        }
                      }}
                    >
                      {a.label}
                      {route ? <span style={{ marginLeft: 6, fontSize: 11 }}>去处理 →</span> : null}
                    </Button>
                  );
                })}
              </Space>
              <div style={{ fontSize: 11.5, color: '#9aa0aa' }}>
                以上为 AI 建议，不是已执行的操作。平台内任何变更都需在对应页面发起，并经过权限校验与审计。
              </div>
            </>
          ) : null}
        </div>
      </div>
    );
  };

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'AI 运维助手' }]}
        title="AI 运维助手"
        subTitle="Beta"
        description="用自然语言查询构建与部署状态，失败自动归因、异常自动推荐处置动作"
        extra={
          <Space>
            <Tooltip title="通道与模型由「平台默认通道」决定，AI 请求本身不支持指定模型">
              <Tag
                color={modelUnknown ? 'default' : usableModel ? (keyMissing ? 'orange' : 'blue') : 'red'}
                className="yn-mono"
                style={{ marginInlineEnd: 0, padding: '4px 10px' }}
              >
                <ApiOutlined style={{ marginRight: 6 }} />
                {modelUnknown
                  ? '通道状态不可见（无 user:manage）'
                  : activeModel
                    ? `${activeModel.name} · ${activeModel.defaultModel}`
                    : '未配置可用模型'}
              </Tag>
            </Tooltip>
            {canSeeModels ? (
              <Tooltip title="管理模型通道">
                <Button icon={<SettingOutlined />} onClick={() => navigate('/system/models')} />
              </Tooltip>
            ) : null}
            {/* 「历史会话」按钮已去掉：会话列表现在**常驻左栏**（2026-10-15），
                再留一个抽屉入口就是同一份数据的第二个开关 —— 两处状态还要互相同步。 */}
            <Button
              icon={<ClearOutlined />}
              disabled={!messages.length}
              onClick={() => {
                newConversation();
                message.success('已清空当前对话');
              }}
            >
              清空
            </Button>
          </Space>
        }
      />

      {modelUnknown ? (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message="无法读取模型通道配置"
          description="当前账号没有 user:manage 权限，读不到「模型设置」里的通道清单。这不影响提问——后端会按平台默认通道执行；若回答以 HTTP 401 之类的错误结束，请让管理员检查模型通道与密钥。"
        />
      ) : !usableModel || keyMissing ? (
        <Alert
          type={!usableModel ? 'error' : 'warning'}
          showIcon
          icon={<WarningOutlined />}
          style={{ marginBottom: 12 }}
          message={
            !usableModel
              ? '当前没有可用的模型通道，AI 助手无法回答'
              : `默认通道「${activeModel?.name}」尚未配置 API Key`
          }
          description={
            !usableModel
              ? '所有通道都被停用或未配置。请到「系统管理 › 模型设置」启用一个通道并填入密钥。'
              : '该通道保留了展示用掩码，但数据库里没有密钥密文，提问会以「模型通道返回 HTTP 401」结束。请到「模型设置」补填密钥后重试。'
          }
          action={
            <Button size="small" type="primary" onClick={() => navigate('/system/models')}>
              去配置
            </Button>
          }
        />
      ) : null}

      <div className="yn-ai-page">
        {/* ---------------------------------------------------- 左：历史会话
            DeepSeek 那种"左边能看到历史"的形态，配色沿用本平台这套浅色 token。
            数据只含**当前账号自己的**会话（后端按登录用户过滤）。 */}
        <aside className="yn-ai-side">
          <Button block icon={<PlusOutlined />} onClick={newConversation}>
            新建对话
          </Button>
          <div className="yn-ai-side__list">
            {convsLoading ? (
              <div className="yn-ai-side__hint">
                <LoadingOutlined /> 正在加载…
              </div>
            ) : convs.length === 0 ? (
              <div className="yn-ai-side__hint">
                还没有历史对话
                <div style={{ marginTop: 4 }}>问第一句时会自动建一条</div>
              </div>
            ) : (
              groupConversations(convs).map(([label, items]) => (
                <div key={label}>
                  <div className="yn-ai-side__group">{label}</div>
                  {items.map((c) => (
                    <div
                      key={c.key}
                      className={`yn-ai-conv${c.key === conversationId ? ' yn-ai-conv--active' : ''}`}
                      title={c.title}
                      onClick={() => void openConversation(c.key)}
                    >
                      <div className="yn-ai-conv__title">
                        {openingKey === c.key ? <LoadingOutlined style={{ marginRight: 6 }} /> : null}
                        {c.title}
                      </div>
                      <div className="yn-ai-conv__meta">
                        <span>{ago(c.lastMsgAt)}</span>
                        {/* 删除按钮必须 stopPropagation：否则点它会连带打开这条会话 */}
                        <span
                          className="yn-ai-conv__del"
                          onClick={(e) => {
                            e.stopPropagation();
                            removeConversation(c.key, c.title);
                          }}
                        >
                          <DeleteOutlined /> 删除
                        </span>
                      </div>
                    </div>
                  ))}
                </div>
              ))
            )}
          </div>
        </aside>

        {/* ---------------------------------------------------- 右：对话 */}
        <div style={{ flex: 1, minWidth: 0 }}>
      <div className="yn-ai-shell">
        <div className="yn-ai-body" ref={bodyRef}>
          {messages.length === 0 ? (
            <div style={{ textAlign: 'center', padding: '48px 0', color: '#9aa0aa' }}>
              <RobotOutlined style={{ fontSize: 40, color: '#d9d9d9' }} />
              <div style={{ marginTop: 12 }}>问我关于构建、部署、回滚的任何问题</div>
              <div style={{ marginTop: 6, fontSize: 12 }}>
                我只具备只读查询能力，不会替你执行任何变更
              </div>
            </div>
          ) : (
            messages.map((m) => (
              <div key={m.id} className={`yn-ai-msg yn-ai-msg--${m.role}`}>
                <div className="yn-ai-avatar">
                  {m.role === 'assistant' ? <RobotOutlined /> : <UserOutlined />}
                </div>
                <div>
                  {m.error ? (
                    <Alert
                      type="error"
                      showIcon
                      style={{ marginBottom: 8 }}
                      message="本次回答中断"
                      description={<span className="yn-mono" style={{ fontSize: 12 }}>{m.error}</span>}
                    />
                  ) : null}

                  {m.tools?.length ? (
                    <div style={{ marginBottom: 8 }}>
                      <div className="yn-ai-tools__title">
                        <ThunderboltOutlined style={{ marginRight: 5 }} />
                        查询过程 · 共 {m.tools.length} 次（模型自己发起的只读查询）
                      </div>
                      {m.tools.map((t, i) => renderTool(t, i))}
                    </div>
                  ) : null}

                  {m.content || m.streaming ? (
                    <div className="yn-ai-bubble">
                      {m.content.split('\n').map((line, i) => (
                        <div key={i}>
                          {line.split(/(`[^`]+`|\*\*[^*]+\*\*)/g).map((seg, j) => {
                            if (seg.startsWith('`') && seg.endsWith('`')) {
                              return (
                                <span
                                  key={j}
                                  className="yn-mono"
                                  style={{ background: '#f4f6f9', padding: '1px 5px', borderRadius: 3, fontSize: 12.5 }}
                                >
                                  {seg.slice(1, -1)}
                                </span>
                              );
                            }
                            if (seg.startsWith('**') && seg.endsWith('**')) {
                              return <b key={j}>{seg.slice(2, -2)}</b>;
                            }
                            return <span key={j}>{seg}</span>;
                          })}
                        </div>
                      ))}
                      {m.streaming ? (
                        <span style={{ color: '#1677ff', marginLeft: 2 }}>▍</span>
                      ) : null}
                    </div>
                  ) : null}

                  {m.attachments?.map((att, ai) => renderAttachment(att, ai))}

                  <div style={{ fontSize: 11.5, color: '#c2c7d0', marginTop: 5 }}>
                    {m.at}
                    {m.role === 'assistant' && m.streaming ? ' · 生成中' : ''}
                  </div>
                </div>
              </div>
            ))
          )}
        </div>

        <div style={{ borderTop: '1px solid #eceef2', paddingTop: 12 }}>
          <div className="yn-ai-suggest">
            {suggestions.map((s) => (
              <span
                className="yn-ai-chip"
                key={s}
                style={{ opacity: streaming ? 0.5 : 1 }}
                onClick={() => (streaming ? undefined : ask(s))}
              >
                <ThunderboltOutlined style={{ marginRight: 5, color: '#1677ff' }} />
                {s}
              </span>
            ))}
          </div>
          <Space.Compact style={{ width: '100%' }}>
            <Input
              size="large"
              placeholder="例如：帮我看看测试区192 demouser 服务现在什么状态"
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={onInputKeyDown}
              // 组词开始/结束。结束时刻意延后一个 tick 复位：
              // 中文输入法确认候选词的那次回车，compositionend 会先于 keydown 到达，
              // 立即复位就等于失去了这道守卫。
              onCompositionStart={() => {
                composingRef.current = true;
              }}
              onCompositionEnd={() => {
                setTimeout(() => {
                  composingRef.current = false;
                }, 0);
              }}
              allowClear
              disabled={streaming}
            />
            {streaming ? (
              <Button size="large" danger icon={<StopOutlined />} onClick={stop}>
                停止
              </Button>
            ) : (
              <Button
                size="large"
                type="primary"
                icon={<SendOutlined />}
                onClick={() => ask(input)}
                loading={false}
                disabled={!input.trim()}
              >
                发送
              </Button>
            )}
          </Space.Compact>
          <div
            style={{
              marginTop: 8,
              display: 'flex',
              alignItems: 'center',
              flexWrap: 'wrap',
              gap: '4px 8px',
              fontSize: 12,
              color: '#b5bac4',
            }}
          >
            {modelUnknown ? (
              <Tag style={{ marginInlineEnd: 0 }}>通道状态不可见</Tag>
            ) : usableModel ? (
              <Tag color={keyMissing ? 'orange' : 'blue'} className="yn-mono" style={{ marginInlineEnd: 0 }}>
                {keyMissing ? '密钥未配置' : '通道就绪'} · {activeModel?.name} · {activeModel?.defaultModel}
              </Tag>
            ) : (
              <Tag color="red" style={{ marginInlineEnd: 0 }}>
                未配置可用模型
              </Tag>
            )}
            {conversationId ? (
              <span className="yn-mono">会话 {conversationId}</span>
            ) : null}
            <span>
              提示：AI 具备只读查询权限，任何变更类操作（部署 / 回滚 / 扩缩容）都会生成待确认卡片，不会自动执行。
            </span>
          </div>
        </div>
      </div>

        </div>
      </div>
    </div>
  );
};

export default AiAssistant;
