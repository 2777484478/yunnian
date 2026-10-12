import React, { useCallback, useEffect, useMemo, useState } from 'react';
import { Alert, Button, Input, Radio, Select, Space, Spin, Tag, Typography } from 'antd';
import { ReloadOutlined } from '@ant-design/icons';
import { ApiError, isGitParam, jenkinsApi } from '@/api';
import type { JenkinsBuildParam, JenkinsParamDef, JenkinsParamValues } from '@/api';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// Jenkins 构建参数：取数 + 渲染
//
// 这套东西原先长在「Jenkins Job 列表页」的构建弹窗里。现在**构建入口统一收到
// 项目管理**（列表页只做浏览），于是把参数区抽成这里的一组可复用件：
//
//   useJenkinsParams()      —— 取参数定义 + git 参数的可选值（带重取）
//   JenkinsGitParamSelect   —— 单个"选版本号/分支"的下拉（标签与分支分组）
//   JenkinsParamFields      —— 其余参数（choice / boolean / string / text / password）
//   collectJenkinsParams()  —— 把选择摊平成提交给 Jenkins 的数组
//
// 四条刻意保留的设计，都是踩过坑的：
//
//   1. **GitParameter 的默认值平台读不到，所以一律留空不传。**
//      插件不通过 API 暴露配置页里的 defaultValue（`tree=*` 也只有 null），
//      真实默认值只出现在 `description` 的文字里（实测是「默认master」）。
//      留空 → 不传该参数 → Jenkins 用它自己配置的默认值，与在 Jenkins 页面上
//      直接点 Build 完全一致；平台猜一个默认值传过去 → 一旦猜错就构建错版本，
//      而且全过程不报错。界面上把 `description` 原文显示出来，让人知道不选会发生什么。
//
//   2. **版本标签与分支分组，不混在一个列表里。**
//      实测一个 job 的可选值是 84 个版本标签 + 227 个远端分支引用（共 311 条）。
//      混排的话，"选版本号"要在 311 条里翻。两组都保留，标签组在前。
//
//   3. **"取到 0 个可选值"必须给出排查方向，不能只写"暂无数据"。**
//      Jenkins 那个接口有静默失败：查询串写成 `name=` 而不是 `param=` 时返回
//      **200 + 空数组**，不报错。所以后端会先读参数定义、参数名对不上就主动 40400，
//      界面再把这层含义说清楚，而不是让用户对着空下拉框发呆。
//
//   4. **平台不认识的参数类型不允许提交。** 猜着渲染成文本框会送错值，
//      所以只读展示 + 说明"请到 Jenkins 页面构建"，并挡住提交。
//
// 性能：可选值那个接口**每次都会去仓库拉一遍**（实测 0.6~1.1s，无明显缓存），
// 所以只在真的要用参数时（打开编译弹窗且项目绑定的是 Jenkins Job）才拉，
// 列表页绝不批量拉——531 个 job 全量拉会把这台 Jenkins 拖慢。
// ---------------------------------------------------------------------------

/** 一个参数的空值快照，用于"取不到值"时也能渲染出可解释的界面 */
export const emptyJenkinsValues = (param: string, errors: string[] = []): JenkinsParamValues => ({
  param,
  tags: [],
  branches: [],
  other: [],
  total: 0,
  errors,
  fetchedAt: new Date().toISOString(),
  elapsedMs: 0,
  source: 'real',
});

/** `useJenkinsParams` 的返回结构 */
export interface JenkinsParamsState {
  /** 参数定义。null = 还没拉到；[] = 拉到了，该 Job 确实没有参数。 */
  defs: JenkinsParamDef[] | null;
  /** 正在读参数定义（或紧接着读可选值） */
  loading: boolean;
  /** 读参数定义失败的原因。失败时 defs 为 null——**不允许在不知道参数的情况下提交**。 */
  error: string | null;
  /** git 参数的可选值，按参数名索引 */
  values: Record<string, JenkinsParamValues>;
  /** 每个 git 参数的加载态 */
  loadingValues: Record<string, boolean>;
  gitDefs: JenkinsParamDef[];
  /** 非 git 参数（含平台不认识的类型，它们要在界面上被明确标出） */
  otherDefs: JenkinsParamDef[];
  unknownDefs: JenkinsParamDef[];
  reload: () => void;
  reloadValues: (d: JenkinsParamDef) => void;
}

/**
 * 取某个 Jenkins Job 的构建参数（含 git 参数的可选值）。
 *
 * `enabled=false` 时清空全部状态并不发请求——弹窗没打开就不该碰 Jenkins。
 * 换 Job（fullName 变了）或重新打开时一律从干净状态开始：
 * 不做重置会看到上一个 Job 的参数、甚至带着上一个 Job 的选择提交，那是会构建错对象的。
 */
export function useJenkinsParams(fullName: string, enabled: boolean): JenkinsParamsState {
  const [defs, setDefs] = useState<JenkinsParamDef[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [values, setValues] = useState<Record<string, JenkinsParamValues>>({});
  const [loadingValues, setLoadingValues] = useState<Record<string, boolean>>({});

  /** 拉 git 参数的可选值。也供"重新读取"复用 */
  const loadValues = useCallback(async (name: string, list: JenkinsParamDef[]) => {
    const gitDefs = list.filter((d) => isGitParam(d.kind));
    if (!gitDefs.length) return;

    setLoadingValues((m) => {
      const next = { ...m };
      gitDefs.forEach((d) => {
        next[d.name] = true;
      });
      return next;
    });

    // 并行拉：多参数时串行会明显变慢，而它们之间没有依赖。
    await Promise.all(
      gitDefs.map(async (d) => {
        try {
          const v = await jenkinsApi.paramValues(name, d.name);
          setValues((m) => ({ ...m, [d.name]: v }));
        } catch (e) {
          // 拉失败**不能**当成"这个参数没有可选值"——后者会让用户以为仓库里没标签。
          // 把错误原样记进 values 的 errors 里，由界面的诊断区展示。
          setValues((m) => ({
            ...m,
            [d.name]: emptyJenkinsValues(d.name, [e instanceof ApiError ? e.message : '读取可选值失败']),
          }));
        } finally {
          setLoadingValues((m) => ({ ...m, [d.name]: false }));
        }
      }),
    );
  }, []);

  const load = useCallback(
    async (name: string) => {
      setLoading(true);
      setError(null);
      try {
        const list = await jenkinsApi.params(name);
        setDefs(list);
        await loadValues(name, list);
      } catch (e) {
        setError(e instanceof ApiError ? e.message : '读取构建参数失败');
        setDefs(null);
      } finally {
        setLoading(false);
      }
    },
    [loadValues],
  );

  useEffect(() => {
    setDefs(null);
    setValues({});
    setLoadingValues({});
    setError(null);
    setLoading(false);
    if (enabled && fullName) void load(fullName);
  }, [enabled, fullName, load]);

  const gitDefs = useMemo(() => (defs ?? []).filter((d) => isGitParam(d.kind)), [defs]);
  const otherDefs = useMemo(() => (defs ?? []).filter((d) => !isGitParam(d.kind)), [defs]);
  const unknownDefs = useMemo(() => (defs ?? []).filter((d) => d.kind === 'unknown'), [defs]);

  const reload = useCallback(() => {
    if (fullName) void load(fullName);
  }, [fullName, load]);

  const reloadValues = useCallback(
    (d: JenkinsParamDef) => {
      void loadValues(fullName, [d]);
    },
    [fullName, loadValues],
  );

  return { defs, loading, error, values, loadingValues, gitDefs, otherDefs, unknownDefs, reload, reloadValues };
}

/**
 * 平台确实读得到默认值的参数（choice/boolean/string 等）预填出来。
 * git 参数的 defaultValue 恒为 null，所以它们自然保持空 → 不传 → 用 Jenkins 的默认值。
 */
export function defaultJenkinsSelection(defs: JenkinsParamDef[]): Record<string, string> {
  const init: Record<string, string> = {};
  for (const d of defs) {
    if (!isGitParam(d.kind) && d.kind !== 'unknown' && d.defaultValue != null) {
      init[d.name] = d.defaultValue;
    }
  }
  return init;
}

/** 把选择摊平成要发给 Jenkins 的参数。留空的参数**不出现**在结果里（= 不传该参数）。 */
export function collectJenkinsParams(
  defs: JenkinsParamDef[],
  selected: Record<string, string | string[]>,
): JenkinsBuildParam[] {
  const out: JenkinsBuildParam[] = [];
  for (const d of defs) {
    const v = selected[d.name];
    if (v === undefined || v === null || v === '') continue;
    if (Array.isArray(v)) {
      // 多选参数：同名参数提交多次，Jenkins 接受这种形态
      v.forEach((item) => {
        if (item) out.push({ name: d.name, value: item });
      });
    } else {
      out.push({ name: d.name, value: v });
    }
  }
  return out;
}

interface GitSelectProps {
  /** 要渲染的 git 参数。null/undefined = 该 Job 没有分支参数（或有别的原因不能选） */
  param?: JenkinsParamDef | null;
  /** 不能选时的占位文案，必须说明原因 */
  emptyPlaceholder?: string;
  values?: JenkinsParamValues;
  loading?: boolean;
  onReload?: () => void;
  /** 由 Form.Item 或父组件注入 */
  value?: string | string[];
  onChange?: (v: string | string[]) => void;
}

/**
 * 一个 git 参数的下拉：把 `git_branch` 这类参数渲染成"可选版本标签 / 分支"的搜索下拉。
 */
export const JenkinsGitParamSelect: React.FC<GitSelectProps> = ({
  param,
  emptyPlaceholder = '不可选择',
  values,
  loading = false,
  onReload,
  value,
  onChange,
}) => {
  if (!param) {
    return <Select disabled placeholder={emptyPlaceholder} style={{ width: '100%' }} />;
  }

  const groups: { label: string; options: { label: string; value: string }[] }[] = [];
  if (values?.tags.length) {
    groups.push({
      label: `版本标签 ${values.tags.length}`,
      options: values.tags.map((x) => ({ label: x, value: x })),
    });
  }
  if (values?.branches.length) {
    groups.push({
      label: `分支 ${values.branches.length}`,
      options: values.branches.map((x) => ({ label: x, value: x })),
    });
  }
  // other 兜底展示：万一有既不像标签也不像分支的值，宁可多显示一组也不要**静默丢掉**
  if (values?.other.length) {
    groups.push({
      label: `其他 ${values.other.length}`,
      options: values.other.map((x) => ({ label: x, value: x })),
    });
  }

  return (
    <div>
      <Select
        showSearch
        allowClear
        // 多选时提交多次同名参数；单选时提交一次。两种都是 Jenkins 接受的形态。
        mode={param.multiple ? 'multiple' : undefined}
        style={{ width: '100%' }}
        // 占位文案刻意写短：这个控件在"项目编译"里只占半栏，长句子会被截成
        // 「不选则使用 Jenkins 侧默认值（该参数说…」，反而看不清。
        // 参数说明放到下面那行信息里去写全。
        placeholder="不选则用 Jenkins 默认值"
        value={value ?? (param.multiple ? [] : undefined)}
        onChange={(val) => onChange?.(val ?? (param.multiple ? [] : ''))}
        loading={loading}
        options={groups}
        optionFilterProp="label"
        // 311 条选项必须能搜索；同时把下拉高度限住，否则会盖住底部的提交按钮
        listHeight={280}
        notFoundContent={
          loading ? (
            <div style={{ padding: 12, textAlign: 'center' }}>
              <Spin size="small" />{' '}
              <Text type="secondary" style={{ fontSize: 12 }}>
                正在从仓库读取…
              </Text>
            </div>
          ) : values && values.total === 0 ? (
            <div style={{ padding: 10 }}>
              <Text type="secondary" style={{ fontSize: 12 }}>
                没有取到可选值，请看下方说明
              </Text>
            </div>
          ) : null
        }
      />

      <div style={{ marginTop: 4 }}>
        {loading ? (
          <Text type="secondary" style={{ fontSize: 11.5 }}>
            正在从仓库读取分支与标签（每次都会真去读一遍，通常 1 秒内）…
          </Text>
        ) : values ? (
          <Space size={8} wrap style={{ fontSize: 11.5 }}>
            <Text type="secondary">
              参数 <Text className="yn-mono">{param.name}</Text> 取到 {values.total} 个可选值
              {values.tags.length || values.branches.length
                ? `（${values.tags.length} 个版本标签 / ${values.branches.length} 个分支）`
                : ''}
              ，耗时 {values.elapsedMs}ms
            </Text>
            {/* 不选会构建出什么，取决于 Jenkins 侧的默认值；平台读不到那个值，
                只能把插件写在说明里的原文转述出来——不能替用户装作知道 */}
            {param.description ? (
              <Text type="secondary">· Jenkins 侧说明：{param.description}</Text>
            ) : null}
            {values.total > 0 ? <Text type="secondary">· 支持搜索，输入版本号片段即可</Text> : null}
            {onReload ? (
              <Button
                type="link"
                size="small"
                icon={<ReloadOutlined />}
                loading={loading}
                onClick={onReload}
                style={{ padding: 0, height: 'auto', fontSize: 11.5 }}
              >
                重新读取
              </Button>
            ) : null}
          </Space>
        ) : null}
      </div>

      {/* 空值 / 报错的诊断区。绝不能只写"暂无数据"——这个接口会静默返回空。 */}
      {!loading && values && (values.total === 0 || values.errors.length > 0) ? (
        <Alert
          style={{ marginTop: 6 }}
          type="warning"
          showIcon
          message={values.errors.length > 0 ? '读取可选值时 Jenkins 报错' : 'Jenkins 没有返回任何可选值'}
          description={
            <div style={{ fontSize: 12, lineHeight: 1.85 }}>
              {values.errors.length > 0 ? (
                <div className="yn-mono" style={{ marginBottom: 4 }}>
                  {values.errors.join('；')}
                </div>
              ) : null}
              <div>常见原因，按可能性排序：</div>
              <div>① 仓库里确实没有标签 / 分支（该 Job 指向的仓库可能刚开始用）；</div>
              <div>
                ② Jenkins 侧读取仓库失败——凭据失效或网络不通，此时 Jenkins 页面上的下拉同样是空的；
              </div>
              <div>③ 参数名与 Jenkins 侧不一致（平台按名字去问 Jenkins 要值）。</div>
              <div style={{ marginTop: 4 }}>
                <Text strong>平台不会在取不到值时替你猜一个版本</Text>
                ——直接提交会构建出错的东西。请先到 Jenkins 页面确认那个下拉是否正常。
              </div>
            </div>
          }
          action={
            onReload ? (
              <Button size="small" onClick={onReload}>
                重新读取
              </Button>
            ) : null
          }
        />
      ) : null}
    </div>
  );
};

interface FieldsProps {
  defs: JenkinsParamDef[];
  value: Record<string, string | string[]>;
  onChange: (name: string, v: string | string[]) => void;
}

/** 参数名 + 类型标签 + 说明 的标题行（各类型共用） */
const fieldHeader = (d: JenkinsParamDef) => (
  <div style={{ marginBottom: 4 }}>
    <Space size={6} wrap>
      <Text strong className="yn-mono" style={{ fontSize: 13 }}>
        {d.name}
      </Text>
      {d.kind === 'unknown' ? (
        <Tag color="orange" style={{ marginInlineEnd: 0 }}>
          平台暂不支持该类型
        </Tag>
      ) : null}
      {d.description ? (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {d.description}
        </Text>
      ) : null}
    </Space>
  </div>
);

/**
 * 非 git 参数的渲染（choice / boolean / string / text / password / 不认识的类型）。
 *
 * 由调用方持有值（`value` + `onChange`），因为它要和 git 参数一起被摊平成
 * 一次提交发给 Jenkins——值散在两处最容易出现"界面上填了、提交时漏了"。
 */
export const JenkinsParamFields: React.FC<FieldsProps> = ({ defs, value, onChange }) => (
  <>
    {defs.map((d) => {
      const v = value[d.name];

      if (d.kind === 'unknown') {
        return (
          <div key={d.name} style={{ marginBottom: 14 }}>
            {fieldHeader(d)}
            <Text type="secondary" style={{ fontSize: 12 }}>
              参数类型为 <Text code>{d.className}</Text>，平台还不认识它。 为避免送错值，这里不允许提交——请到
              Jenkins 页面用 Build with Parameters 构建。
            </Text>
          </div>
        );
      }

      if (d.kind === 'boolean') {
        return (
          <div key={d.name} style={{ marginBottom: 14 }}>
            {fieldHeader(d)}
            <Radio.Group
              value={typeof v === 'string' && v !== '' ? v : 'false'}
              onChange={(e) => onChange(d.name, e.target.value)}
              options={[
                { label: 'true', value: 'true' },
                { label: 'false', value: 'false' },
              ]}
              optionType="button"
              size="small"
            />
          </div>
        );
      }

      if (d.kind === 'choice') {
        return (
          <div key={d.name} style={{ marginBottom: 14 }}>
            {fieldHeader(d)}
            <Select
              style={{ width: '100%' }}
              value={typeof v === 'string' && v !== '' ? v : undefined}
              onChange={(val) => onChange(d.name, val)}
              options={(d.choices ?? []).map((c) => ({ label: c, value: c }))}
              placeholder="请选择"
            />
          </div>
        );
      }

      const text = typeof v === 'string' ? v : '';

      return (
        <div key={d.name} style={{ marginBottom: 14 }}>
          {fieldHeader(d)}
          {d.kind === 'text' ? (
            <Input.TextArea rows={3} value={text} onChange={(e) => onChange(d.name, e.target.value)} />
          ) : d.kind === 'password' ? (
            // 密码类参数的默认值可能来自 Jenkins 凭据，平台不预填、也不回显
            <Input.Password
              value={text}
              onChange={(e) => onChange(d.name, e.target.value)}
              placeholder={d.defaultValue ? '留空使用 Jenkins 侧的值' : '请输入'}
            />
          ) : (
            <Input value={text} onChange={(e) => onChange(d.name, e.target.value)} />
          )}
        </div>
      );
    })}
  </>
);
