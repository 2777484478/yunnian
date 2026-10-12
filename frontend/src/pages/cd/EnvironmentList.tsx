import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  AutoComplete,
  Badge,
  Button,
  Col,
  Divider,
  Drawer,
  Form,
  Input,
  InputNumber,
  Row,
  Segmented,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  AppstoreOutlined,
  CloudServerOutlined,
  EditOutlined,
  ExportOutlined,
  PlusOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  SyncOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons';
import { Link, useNavigate } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { UsageBar } from '@/components/charts';
import { EnvStatusBadge, MethodTag } from '@/components/StatusTag';
import { DisabledTip } from '@/components/DisabledTip';
import { KVFields } from '@/components/KVFields';
import { clusterApi, environmentApi, userApi } from '@/api';
import type { EnvProjectCandidate, EnvResources } from '@/api/cd';
import {
  OWN_CLUSTER_BADGE,
  OWN_CLUSTER_HINT,
  clusterBadgeOf,
  clusterKeyOf,
  isOwnCluster,
} from '@/config/topology';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { envResStatusOf, envUsageOf, useEnvResources } from '@/hooks/useEnvResources';
import { isServiceAbnormal, liveWordOf } from '@/utils/live';
import type { Environment, EnvService, EnvVar } from '@/types';

const { Text } = Typography;

/**
 * 新建/编辑抽屉的表单值。与后端 `EnvironmentInput` 逐字段对齐
 * （`EnvironmentList` 是环境的**唯一写入入口**，字段名对不上就会静默丢弃）。
 *
 * 「新建」只渲染建环境真正必需的字段；带 * 的几项依赖"环境已经存在"，
 * 仅在编辑时出现 —— 最典型的是 `projectKeys`：纳管项目要往 `service_instances`
 * 里建行，而那个表的外键指向环境，环境还没建出来就无从谈起。
 */
interface EnvFormValues {
  name: string;
  key?: string;
  cluster: string;
  clusterVersion?: string;
  namespace: string;
  deployMethod: Environment['deployMethod'];
  // —— 新建/编辑都有 ——
  owner?: string;
  needApproval?: boolean;
  approvers?: string[];
  ownKubeconfig?: boolean;
  kubeconfig?: string;
  stageLabel?: string;
  stageOrder?: number;
  description?: string;
  // —— 仅编辑（* 环境存在之后才谈得上的配置）——
  registry?: string;
  ingressDomain?: string;
  enabled?: boolean;
  projectKeys?: string[];
  vars?: EnvVar[];
}

const EnvironmentList: React.FC = () => {
  const navigate = useNavigate();
  const { message, modal } = AntApp.useApp();
  const canWrite = hasPerm('env:write');

  const [view, setView] = useState<'card' | 'table'>('card');
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [syncing, setSyncing] = useState(false);

  // keyword 落成请求要防抖：这个页面一次拿全量环境（数量少），
  // 但每敲一个字仍然会打一次接口，没有防抖会白刷十几次。
  useEffect(() => {
    const t = setTimeout(() => setKeyword(keywordInput.trim()), 300);
    return () => clearTimeout(t);
  }, [keywordInput]);

  // ------------------------------------------------------------------ 取数
  const envApi = useApi(() => environmentApi.list({ keyword, pageSize: 100 }), [keyword]);
  const envs = useMemo(() => envApi.data?.list ?? [], [envApi.data]);

  // 集群候选来自「系统管理 › 集群接入」里**已接入的集群** —— 不再写死三套常量
  // （原先那套与 topology.ts 的 CLUSTER_META 重复且会各自漂移），也不从环境列表反推
  // （反推会把"某个环境手输错的集群名"当成合法候选，一路复制下去）。
  // 集群接口还没实现时列表为空，AutoComplete 退化为自由输入，环境照样能维护。
  const clustersApi = useApi(() => clusterApi.list(), []);
  const clusterOptions = useMemo(() => {
    const fromPlatform = (clustersApi.data ?? []).map((c) => ({
      label: `${c.name} · ${clusterKeyOf(c.key)}${
        c.status === 'connected' ? '' : `（${clusterBadgeOf(c.status)}）`
      }`,
      value: clusterKeyOf(c.key),
    }));
    // 存量环境里的集群标识可能不在集群表里（集群实体化之前手输的值），
    // 这些也要列出来，否则编辑环境时连"当前所属集群"都看不到。
    //
    // 但后缀不能一律写"未接入平台"：集群接口挂掉时所有人都对不上，
    // 那不是"没接入"，是"没读到"。两种情况的后缀分开写。
    const suffix = clustersApi.error ? '（集群列表读取失败）' : '（未接入平台）';
    const known = new Set(fromPlatform.map((o) => o.value));
    const legacy = Array.from(new Set(envs.map((e) => clusterKeyOf(e.cluster)).filter(Boolean)))
      .filter((c) => !known.has(c))
      .sort()
      .map((c) => ({ label: `${c}${suffix}`, value: c }));
    return [...fromPlatform, ...legacy];
  }, [clustersApi.data, clustersApi.error, envs]);

  // 卡片上要显示"该项目下有几个服务 / 几个待关注"，环境接口本身不带这个聚合值。
  // 环境数量是个位数，每个环境打一次 /services 比让后端为列表页加一个聚合字段更划算，
  // 也避免为原型改接口契约。keys 用 join 当依赖，避免数组引用变化导致重复请求。
  const envKeys = useMemo(() => envs.map((e) => e.key).sort().join(','), [envs]);
  const servicesApi = useApi(
    async () => {
      const keys = envKeys ? envKeys.split(',') : [];
      const groups = await Promise.all(keys.map((k) => environmentApi.services(k)));
      return groups.flat();
    },
    [envKeys],
    { enabled: !!envKeys },
  );
  const allServices = useMemo(() => servicesApi.data ?? [], [servicesApi.data]);
  const servicesOf = (envKey: string): EnvService[] => allServices.filter((s) => s.envKey === envKey);

  /**
   * 卡片 / 表格上的 CPU、内存与「资源状态」**一律读集群实时值**。
   *
   * 原先用的是环境快照（`environment.cpu_usage / mem_usage`）：它由「同步集群」写入，
   * 而列表页没有任何定时刷新 —— 于是卡片把几小时前的旧数当"现在"显示。
   * 实测同一个环境：卡片内存 96%，点进详情页实时是 93%（CPU 6% / 8%）。
   * 两个数**都没算错** —— 后端两次调用走的是同一段 `Nodes()`，差的只有时间点。
   * 真正的问题是把快照当现状展示，且一个字的时间标识都没有。
   *
   * 取数与阈值推导都在 `useEnvResources` 里（概览页、详情页共用同一份口径）。
   */
  const envRes = useEnvResources(envs.map((e) => e.key));
  const liveResOf = (envKey: string): EnvResources | null => envRes.map?.get(envKey) ?? null;
  const usageOf = (envKey: string): EnvResources | null => envUsageOf(envRes.map, envKey);
  const resStatusOf = (envKey: string): Environment['status'] => envResStatusOf(envRes.map, envKey);

  // 平台用户（owner / 审批人选择器）。后端把 owner、approvers 都按**用户名**解析成 ID，
  // 所以这里必须提交 username，不能提交展示名。
  //
  // 走 `/users/options`（权限 `env:write`）而不是 `/users`（要 `user:manage`）：
  // 后者会让只有 `env:write` 的 ops 拿到 40300、候选恒为空 —— 抽屉能打开却一个人都选不到。
  const usersApi = useApi(() => userApi.options(), []);
  const userOptions = useMemo(
    () => (usersApi.data ?? []).map((u) => ({ label: `${u.name}（${u.username}）`, value: u.username })),
    [usersApi.data],
  );
  /**
   * 展示名 → 用户名。**只给编辑表单的「负责人」预填用**。
   *
   * `EnvironmentDTO.owner` 回的是**展示名**（卡片上"负责人 运维示例"直接用它），
   * 而选择器的 `value` 是**用户名** —— 两个标识对不上，预填过去的下拉里根本没有这一项，
   * 于是 AntD 只能把原值当标签渲染（用户看到"运维示例"而不是"运维示例（ops）"，
   * 打开下拉也不会被选中）。审批人走的是后端专门回的 `approverUsernames`，
   * 负责人没有对应字段，所以在这里换一次。
   * 换不到就原样保留（值本身可能就是用户名，或者用户列表还没回来）—— 后端
   * `resolveOwner` 按 `name = ? OR username = ?` 查，两种写法都能存。
   */
  const usernameByName = useMemo(() => {
    const m = new Map<string, string>();
    (usersApi.data ?? []).forEach((u) => m.set(u.name, u.username));
    return m;
  }, [usersApi.data]);

  const refreshAll = () => {
    envApi.refresh();
    servicesApi.refresh();
    // 必须跟着刷：卡片上的 CPU/内存/资源状态现在取的是实时值，
    // 不刷它就会出现"点了同步集群、徽标却还是旧的"。
    envRes.refresh();
  };

  // ------------------------------------------------------------------ 统计
  const stats = useMemo(() => {
    const healthy = envs.filter((e) => resStatusOf(e.key) === 'healthy').length;
    const warning = envs.filter((e) => resStatusOf(e.key) === 'warning').length;
    const error = envs.filter((e) => resStatusOf(e.key) === 'error').length;
    const unknown = envs.filter((e) => resStatusOf(e.key) === 'unknown').length;
    // 「待关注服务」只认**集群实时状态**，与「环境详情」同口径。
    //
    // 原先数的是平台库的 `service_instance.status` —— 那是纳管时按默认值写入的期望，
    // 之后只有部署成功才更新。实测同一个环境：平台记 `status=pending`、`image` 还是空串，
    // 集群里其实 running 1/1 —— 于是卡片对着一个健康服务报了"1 个待关注"，
    // 而用户点进详情页看到的是"运行正常 3 个"。一句假警报，比不显示更糟。
    const abnormal = allServices.filter(isServiceAbnormal).length;
    const svcUnknown = allServices.filter((s) => liveWordOf(s) === 'unknown').length;
    return {
      total: envs.length,
      healthy,
      warning,
      error,
      unknown,
      services: allServices.length,
      abnormal,
      svcUnknown,
    };
    // resStatusOf / svcWordOf 每次渲染都是新函数，依赖用它们真正读到的两份数据。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [envs, allServices, envRes.map]);

  // ------------------------------------------------------------------ 同步集群
  // 后端是「按环境」同步（POST /environments/{key}/sync），列表页的"同步集群"
  // 就是把它跑一遍。串行而不是 Promise.all：sync 会去改库、可能触发集群调用，
  // 一串并发打过去对真实集群不友好，而且失败时看不出是哪个环境挂了。
  const syncAll = async () => {
    if (!envs.length) return;
    setSyncing(true);
    const failed: string[] = [];
    for (const e of envs) {
      try {
        await environmentApi.sync(e.key);
      } catch {
        failed.push(e.name);
      }
    }
    setSyncing(false);
    refreshAll();
    if (failed.length) message.warning(`${failed.join('、')} 同步失败，其余环境已更新`);
    else message.success(`已同步 ${envs.length} 个环境的集群状态`);
  };

  // ------------------------------------------------------------------ 导出
  // 原型期的"导出"按钮之前是纯摆设。这里改成真实的 CSV 下载：数据已经在手上，
  // 不值得为它加一个后端接口。
  const exportCsv = () => {
    const head = ['环境标识', '环境名称', 'K8s集群', 'Namespace', '部署方式', '项目数', '服务数', 'CPU%', '内存%', '负责人', '状态', '启用'];
    const rows = envs.map((e) => [
      e.key,
      e.name,
      e.cluster,
      e.namespace,
      e.deployMethod,
      String(e.projectKeys.length),
      String(servicesOf(e.key).length),
      // 导出与页面上是同一份数：实时读到的写数字，读不到留空。
      // 填快照值（或填 0）都会让拿到表格的人以为那就是当时的集群水位。
      usageOf(e.key) ? String(usageOf(e.key)!.cpuUsage) : '',
      usageOf(e.key) ? String(usageOf(e.key)!.memUsage) : '',
      e.owner,
      resStatusOf(e.key),
      e.enabled ? '启用' : '停用',
    ]);
    const csv = [head, ...rows].map((r) => r.map((c) => `"${String(c).replace(/"/g, '""')}"`).join(',')).join('\n');
    const url = URL.createObjectURL(new Blob([`\uFEFF${csv}`], { type: 'text/csv;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = `yunnian-environments-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(url);
    message.success(`已导出 ${envs.length} 条环境记录`);
  };

  // ------------------------------------------------------------------ 抽屉
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editTarget, setEditTarget] = useState<Environment | null>(null);
  const [saving, setSaving] = useState(false);
  const [form] = Form.useForm<EnvFormValues>();
  // 「独立 kubeconfig」开关：打开后才渲染凭据输入框。
  // 用 useWatch 取值而不是拿 shouldUpdate 包一层，避免整块 Row 在每次击键时重渲染。
  const ownKube = Form.useWatch('ownKubeconfig', form) as boolean | undefined;

  // 编辑时要回填环境变量（它不在环境对象上，得单独取 /environments/{key}/vars）
  const varsApi = useApi(() => environmentApi.vars(editTarget!.key), [editTarget?.key], {
    enabled: !!editTarget,
  });

  // 编辑时的项目候选（新建时还没有 key，无法调候选接口）
  const candApi = useApi(() => environmentApi.projectCandidates(editTarget!.key), [editTarget?.key], {
    enabled: !!editTarget,
  });
  const candidates: EnvProjectCandidate[] = useMemo(() => candApi.data ?? [], [candApi.data]);

  const openCreate = () => {
    setEditTarget(null);
    form.resetFields();
    form.setFieldsValue({
      deployMethod: 'k8s',
      enabled: true,
      needApproval: false,
      approvers: [],
      projectKeys: [],
      ownKubeconfig: false,
      vars: [{ key: 'TZ', value: 'Asia/Shanghai' }],
    });
    setDrawerOpen(true);
  };

  const openEdit = (env: Environment) => {
    setEditTarget(env);
    form.setFieldsValue({
      name: env.name,
      key: env.key,
      cluster: env.cluster,
      clusterVersion: env.clusterVersion,
      namespace: env.namespace,
      deployMethod: env.deployMethod,
      registry: env.registry,
      ingressDomain: env.ingressDomain,
      // 负责人：DTO 回的是**展示名**，而选择器的 value 是用户名 → 换一次再预填，
      // 否则这个值在下拉的选项里不存在（见 `usernameByName`）。
      owner: usernameByName.get(env.owner) ?? env.owner,
      needApproval: env.needApproval,
      // 审批人用**用户名**预填（`approverUsernames` 与 `approvers` 一一对应，是后端
      // 专门为这个表单回的）。以前只回展示名、这里只能留空让人重选 ——
      // 于是"打开编辑不重选"就等于把审批人清空，needApproval 时更会被下面的必填规则
      // 拦住、逼用户每次重新点一遍。
      approvers: env.approverUsernames ?? [],
      projectKeys: env.projectKeys,
      enabled: env.enabled,
      description: env.description,
      stageLabel: env.stageLabel,
      stageOrder: env.stageOrder,
      // 凭据只回「有没有配」：开关反映现状，但**不回填内容**（明文没有读出口）。
      // 用户不重新粘贴 ⇒ 提交时 kubeconfig 为空 ⇒ 后端按「不修改凭据」处理。
      ownKubeconfig: env.clusterSource === 'own' || !!env.hasKubeconfig,
      kubeconfig: '',
      vars: [],
    });
    setDrawerOpen(true);
  };

  // 变量取回后补进表单（不能放在 openEdit 里——那边拿不到异步结果）
  useEffect(() => {
    if (!drawerOpen || !editTarget || !varsApi.data) return;
    form.setFieldsValue({ vars: varsApi.data });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [varsApi.data, drawerOpen, editTarget]);

  const submit = async () => {
    const v = await form.validateFields();
    setSaving(true);
    try {
      const body = {
        key: v.key,
        name: v.name,
        cluster: v.cluster,
        clusterVersion: v.clusterVersion,
        namespace: v.namespace,
        deployMethod: v.deployMethod,
        registry: v.registry,
        ingressDomain: v.ingressDomain,
        owner: v.owner,
        needApproval: v.needApproval,
        // 审批人不按 needApproval 清空：关掉审批不该顺手把已配的人删掉
        // （再打开时又得重新点一遍）。优先用表单里的值，拿不到时退回编辑目标上的现值
        // —— needApproval=false 时那个 Form.Item 压根不渲染，
        // "没渲染"不等于"用户清空了"。
        approvers: v.approvers ?? editTarget?.approverUsernames ?? [],
        projectKeys: v.projectKeys ?? [],
        enabled: v.enabled,
        description: v.description,
        stageLabel: v.stageLabel,
        stageOrder: v.stageOrder,
        // 集群凭据来源必须**显式**回传：后端只在收到 'own' / 'platform' 时才动凭据，
        // 空串表示"没提这件事"（留给其他调用方）。漏传的后果是开关点了没反应。
        clusterSource: v.ownKubeconfig ? ('own' as const) : ('platform' as const),
        // 只在勾了自带且**真的粘了内容**时才回传；留空 = 保持原有凭据不变
        ...(v.ownKubeconfig && v.kubeconfig ? { kubeconfig: v.kubeconfig } : {}),
      };

      const saved = editTarget
        ? await environmentApi.update(editTarget.key, body)
        : await environmentApi.create(body);

      // 环境变量走独立接口（覆盖式）。它在环境创建之后才能存——因为路径里有 key。
      // 变量保存失败不回滚环境本身：环境已经建出来了，变量让用户重存一次比丢环境好。
      const key = saved?.key || v.key;
      if (key && v.vars) {
        try {
          await environmentApi.saveVars(key, v.vars.filter((x) => x?.key));
        } catch (e) {
          message.warning(`环境已保存，但环境变量未写入：${(e as Error).message}`);
        }
      }

      message.success(editTarget ? `环境「${v.name}」已保存` : `环境「${v.name}」已创建`);
      setDrawerOpen(false);
      refreshAll();
    } finally {
      setSaving(false);
    }
  };

  const removeEnv = (env: Environment) => {
    // 后端是硬约束而非建议：DELETE /environments/:key 在环境内仍有服务实例时直接 40901 拒绝。
    // 本页已加载了各环境的服务（servicesApi），据此做前端预检——有实例时直接锁住"确认删除"，
    // 把"能不能删"提前讲清楚，而不是让用户点下去才吃一个 409。
    // 服务列表未成功加载时不预判（退回确定性文案），避免用不完整数据误锁。
    const known = !servicesApi.error && !servicesApi.loading;
    const svcCount = servicesOf(env.key).length;
    const blocked = known && svcCount > 0;
    return modal.confirm({
      title: `确认删除环境「${env.name}」？`,
      content: (
        <div style={{ fontSize: 13, lineHeight: 1.9 }}>
          删除后该环境下的部署与回滚记录会保留，但环境配置不可恢复。
          <div style={{ color: '#ff4d4f', marginTop: 6 }}>
            {blocked
              ? `该环境下还有 ${svcCount} 个服务实例，须先下线全部服务再删除环境。`
              : '该环境下若仍有服务实例将无法删除，须先下线全部服务。'}
          </div>
        </div>
      ),
      okText: '确认删除',
      okButtonProps: { danger: true, disabled: blocked },
      onOk: async () => {
        await environmentApi.remove(env.key);
        message.success(`环境「${env.name}」已删除`);
        refreshAll();
      },
    });
  };

  // ------------------------------------------------------------------ 表格列
  const columns: ColumnsType<Environment> = [
    {
      title: '环境名称',
      dataIndex: 'name',
      width: 200,
      fixed: 'left',
      render: (v, r) => (
        <div>
          <Link to={`/cd/environments/${r.key}`} style={{ fontWeight: 500 }}>
            {v}
          </Link>
          <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>
            {r.key} · {r.namespace}
          </div>
        </div>
      ),
    },
    // 标题**不能叫「健康状态」**：这一列的取值只由该环境所在集群的**节点 CPU / 内存水位**
    // 决定（后端 `cd.go` 的 SyncEnvironment：≥90 → error、≥75 → warning、读不到 → unknown），
    // 与服务健康无关。叫「健康状态」会让人以为"这个环境出故障了"——
    // 与上一轮改掉的 ENV_MAP 文案（异常 → 资源紧张）是同一处口径。
    // 取 `resStatusOf`（实时）而不是 `r.status`（上次同步的快照）：同一个环境，
    // 快照可能在几小时前就写下了"资源紧张"，而集群现在早已回落。
    { title: '资源状态', key: 'resStatus', width: 108, render: (_, r) => <EnvStatusBadge status={resStatusOf(r.key)} /> },
    {
      title: 'K8s 集群 / Namespace',
      dataIndex: 'cluster',
      width: 260,
      render: (v, r) => (
        <div>
          <div>
            {v}
            {/* 自带凭据时 `cluster` 只是个标签：后端会绕过平台集群表、直接用环境那份
                凭据，注册表里同名的记录**是另一台机器**。不标出来，两个不同的集群
                会以同一个名字并列在列表里（实测：test-152 与 test 都叫 cluster-local，
                实际一个是 v1.23.10、一个是 v1.22.12）。 */}
            {isOwnCluster(r) ? (
              <Tooltip title={OWN_CLUSTER_HINT}>
                <Tag style={{ marginInlineStart: 6, marginInlineEnd: 0 }}>{OWN_CLUSTER_BADGE}</Tag>
              </Tooltip>
            ) : null}
          </div>
          <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>
            {r.namespace} · {r.clusterVersion}
          </div>
        </div>
      ),
    },
    { title: '部署方式', dataIndex: 'deployMethod', width: 140, render: (v) => <MethodTag method={v} /> },
    {
      title: '服务 / 项目',
      key: 'svc',
      width: 130,
      render: (_, r) => (
        <span>
          <b>{r.projectKeys.length}</b> 个项目 · {servicesOf(r.key).length} 个服务
        </span>
      ),
    },
    {
      title: '资源用量',
      key: 'usage',
      width: 240,
      render: (_, r) => {
        // 与卡片同一份**实时**数据（原先这里是环境快照，会和点进去看到的对不上）。
        const u = usageOf(r.key);
        if (!u) {
          // 读不到就不画条、也不填 0：一条 0% 的灰条会被读成"这份集群很闲"。
          return (
            <span style={{ fontSize: 12, color: '#9aa0aa' }}>
              {envRes.error
                ? '读不到集群用量'
                : envRes.loading
                  ? '正在读取…'
                  : '集群未提供用量指标'}
            </span>
          );
        }
        return (
          <Space direction="vertical" size={4} style={{ width: '100%' }}>
            <div style={{ fontSize: 12, color: '#5c6470' }}>
              CPU {u.cpuUsage}% · 内存 {u.memUsage}%
            </div>
            <div style={{ display: 'flex', gap: 4 }}>
              <div style={{ flex: 1, height: 5, borderRadius: 3, background: '#f0f2f5', overflow: 'hidden' }}>
                <div style={{ width: `${u.cpuUsage}%`, height: '100%', background: u.cpuUsage > 85 ? '#ff4d4f' : '#1677ff' }} />
              </div>
              <div style={{ flex: 1, height: 5, borderRadius: 3, background: '#f0f2f5', overflow: 'hidden' }}>
                <div style={{ width: `${u.memUsage}%`, height: '100%', background: u.memUsage > 85 ? '#ff4d4f' : '#722ed1' }} />
              </div>
            </div>
          </Space>
        );
      },
    },
    {
      title: '审批',
      dataIndex: 'needApproval',
      width: 130,
      render: (v: boolean, r) =>
        v ? (
          <Tooltip title={`审批人：${r.approvers.join('、') || '未指定'}`}>
            <Tag color="orange" icon={<SafetyCertificateOutlined />} style={{ marginInlineEnd: 0 }}>
              需审批
            </Tag>
          </Tooltip>
        ) : (
          <Tag style={{ marginInlineEnd: 0 }}>免审批</Tag>
        ),
    },
    { title: '负责人', dataIndex: 'owner', width: 96 },
    { title: '状态', dataIndex: 'enabled', width: 92, render: (v: boolean) => <Badge status={v ? 'success' : 'default'} text={v ? '启用' : '停用'} /> },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 190,
      render: (_, r) => (
        <Space size={2}>
          <Button type="link" size="small" style={{ paddingInline: 4 }} onClick={() => navigate(`/cd/environments/${r.key}`)}>
            进入
          </Button>
          <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
            <Button
              type="link"
              size="small"
              icon={<EditOutlined />}
              style={{ paddingInline: 4 }}
              disabled={!canWrite}
              onClick={() => openEdit(r)}
            >
              编辑
            </Button>
          </DisabledTip>
          <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
            <Button
              type="link"
              size="small"
              danger
              style={{ paddingInline: 4 }}
              disabled={!canWrite}
              onClick={() => removeEnv(r)}
            >
              删除
            </Button>
          </DisabledTip>
        </Space>
      ),
    },
  ];

  const pageError = envApi.error || servicesApi.error;

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CD 环境部署' }, { title: '环境管理' }]}
        title="环境管理"
        description="维护测试 / 预发布 / 灰度 / 正式等部署环境，配置集群、Namespace 与部署方式"
        extra={
          <Space>
            <Segmented
              value={view}
              onChange={(v) => setView(v as 'card' | 'table')}
              options={[
                { value: 'card', icon: <AppstoreOutlined /> },
                { value: 'table', icon: <UnorderedListOutlined /> },
              ]}
            />
            <Button icon={<ExportOutlined />} onClick={exportCsv} disabled={!envs.length}>
              导出
            </Button>
            <DisabledTip title={!canWrite ? '当前账号没有 env:write 权限' : envs.length ? undefined : '暂无可同步的环境'}>
              <Button icon={<SyncOutlined />} onClick={syncAll} loading={syncing} disabled={!canWrite || !envs.length}>
                同步集群
              </Button>
            </DisabledTip>
            <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
              <Button type="primary" icon={<PlusOutlined />} onClick={openCreate} disabled={!canWrite}>
                新建环境
              </Button>
            </DisabledTip>
          </Space>
        }
      />

      {pageError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="环境数据加载失败"
          description={pageError.message}
          action={
            <Button size="small" onClick={refreshAll}>
              重试
            </Button>
          }
        />
      ) : null}

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard title="环境总数" value={stats.total} unit="个" icon={<CloudServerOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="资源正常环境"
            value={`${stats.healthy}/${stats.total}`}
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                资源告警 {stats.warning} · 资源紧张 {stats.error}
                {stats.unknown ? ` · ${stats.unknown} 个读取不到` : ''}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="纳管服务"
            value={stats.services}
            unit="个"
            accent="#722ed1"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>跨 {stats.total} 个环境的部署实例</Text>}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="待关注服务"
            value={stats.abnormal}
            unit="个"
            accent="#ff4d4f"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                异常 / 部署中 / 已停止
                {stats.svcUnknown ? ` · ${stats.svcUnknown} 个状态读取不到` : ''}
              </Text>
            }
          />
        </Col>
      </Row>

      <div style={{ marginBottom: 12 }}>
        <Input.Search
          allowClear
          placeholder="搜索环境名称 / 标识 / Namespace / 集群"
          style={{ width: 340 }}
          value={keywordInput}
          onChange={(e) => setKeywordInput(e.target.value)}
        />
      </div>

      {!envs.length && !envApi.loading && !pageError ? (
        <SectionCard>
          <div style={{ padding: '32px 0', textAlign: 'center', color: '#9aa0aa' }}>
            {keyword ? `没有匹配「${keyword}」的环境` : '还没有环境，点右上角「新建环境」开始接入集群'}
          </div>
        </SectionCard>
      ) : view === 'card' ? (
        <Row gutter={[16, 16]}>
          {envs.map((env) => {
            const svcs = servicesOf(env.key);
            // 判据全部来自**集群实时状态**（`live`），不是平台库里的 `service_instance.status`。
            const bad = svcs.filter(isServiceAbnormal);
            const svcUnknown = svcs.filter((s) => liveWordOf(s) === 'unknown').length;
            const r = liveResOf(env.key);
            const usage = usageOf(env.key);
            return (
              <Col key={env.key} xs={24} md={12} xl={8}>
                <div className="yn-env-card" onClick={() => navigate(`/cd/environments/${env.key}`)}>
                  <div className="yn-env-card__head">
                    <div>
                      <div className="yn-env-card__name">
                        {env.name}
                        {env.needApproval ? (
                          <Tooltip title={`审批人：${env.approvers.join('、') || '未指定'}`}>
                            <Tag color="orange" icon={<SafetyCertificateOutlined />} style={{ marginInlineEnd: 0 }}>
                              需审批
                            </Tag>
                          </Tooltip>
                        ) : null}
                        {!env.enabled ? <Tag style={{ marginInlineEnd: 0 }}>已停用</Tag> : null}
                      </div>
                      <div className="yn-env-card__key">
                        <span className="yn-mono">{env.key}</span>
                        <Divider type="vertical" style={{ margin: '0 6px' }} />
                        <span className="yn-mono">{env.namespace}</span>
                      </div>
                    </div>
                    {/* 徽标与下面的 CPU/内存取的是**同一份实时数据**：两处若各取一处
                        （徽标读快照、数字读实时），就会出现"数字写着 93%、徽标却按
                        几小时前的 96% 判成资源紧张"这种自相矛盾的卡片。 */}
                    <EnvStatusBadge status={resStatusOf(env.key)} />
                  </div>

                  <div className="yn-env-card__body">
                    <div style={{ fontSize: 13, color: '#5c6470' }}>
                      {env.cluster}
                      {isOwnCluster(env) ? (
                        <Tooltip title={OWN_CLUSTER_HINT}>
                          <Tag style={{ marginInlineStart: 6, marginInlineEnd: 0 }}>
                            {OWN_CLUSTER_BADGE}
                          </Tag>
                        </Tooltip>
                      ) : null}
                      <Text type="secondary" style={{ fontSize: 12, marginLeft: 6 }}>
                        {env.clusterVersion}
                      </Text>
                    </div>
                    <div style={{ marginTop: 10 }}>
                      <Space size={6} wrap>
                        <MethodTag method={env.deployMethod} />
                        {/* 节点数读得到就用**集群实时的**（这个值跟 CPU/内存同一次请求回来，
                            不受 metrics-server 缺失影响）；整条请求失败才退到环境快照
                            —— 那时下面的用量行会同时显示"读不到集群用量"，不会让人误以为
                            这是一份新鲜数据。 */}
                        <Tag style={{ marginInlineEnd: 0 }}>{r?.nodes ?? env.nodes} 节点</Tag>
                        <Tag style={{ marginInlineEnd: 0 }}>{svcs.length} 个服务</Tag>
                        {/* 该环境没有纳管服务时不给任何结论：既不是"全部正常"
                            （没有东西可以正常），也不该报"待关注"。 */}
                        {!svcs.length ? null : bad.length ? (
                          <Tag color="error" style={{ marginInlineEnd: 0 }}>
                            {bad.length} 个待关注
                          </Tag>
                        ) : svcUnknown ? (
                          <Tooltip title="这些服务没能读到集群状态，未纳入判断">
                            <Tag color="warning" style={{ marginInlineEnd: 0 }}>
                              {svcUnknown} 个状态读取不到
                            </Tag>
                          </Tooltip>
                        ) : (
                          <Tag color="success" style={{ marginInlineEnd: 0 }}>
                            全部正常
                          </Tag>
                        )}
                      </Space>
                    </div>

                    {/* 读不到集群用量时显示"—"而不是 0%：一个 0 会被读成"这份集群很闲"，
                        而实际是"没读到"。与「环境详情」同一条纪律，那边连进度条都不画。 */}
                    <div className="yn-env-card__metrics">
                      <div className="yn-env-card__metric">
                        <div className="yn-env-card__metric-label">CPU</div>
                        <div className="yn-env-card__metric-value" style={{ color: usage && usage.cpuUsage > 85 ? '#ff4d4f' : '#2c313a' }}>
                          {usage ? `${usage.cpuUsage}%` : '—'}
                        </div>
                      </div>
                      <div className="yn-env-card__metric">
                        <div className="yn-env-card__metric-label">内存</div>
                        <div className="yn-env-card__metric-value" style={{ color: usage && usage.memUsage > 85 ? '#ff4d4f' : '#2c313a' }}>
                          {usage ? `${usage.memUsage}%` : '—'}
                        </div>
                      </div>
                      <div className="yn-env-card__metric">
                        <div className="yn-env-card__metric-label">项目</div>
                        <div className="yn-env-card__metric-value">{env.projectKeys.length}</div>
                      </div>
                    </div>

                    {usage ? (
                      <div style={{ marginTop: 14 }}>
                        <UsageBar label="CPU 用量" percent={usage.cpuUsage} />
                        <div style={{ marginBottom: 0 }}>
                          <UsageBar label="内存用量" percent={usage.memUsage} />
                        </div>
                      </div>
                    ) : (
                      <div style={{ marginTop: 14, fontSize: 12, color: '#9aa0aa' }}>
                        {envRes.error
                          ? '读不到集群用量'
                          : envRes.loading
                            ? '正在读取集群用量…'
                            : '集群未提供用量指标（需安装 metrics-server）'}
                      </div>
                    )}
                  </div>

                  {/* 「编辑」原先只存在于**表格视图**的操作列，而本页默认落在**卡片视图**上
                      —— 于是"去编辑里把项目纳管进来"这句话在默认视图里无处可点（用户 2026-09-29 反馈
                      "哪里有编辑？我没看到啊"）。点卡片只会进详情页，而详情页没有环境级编辑入口，
                      所以必须把入口补在卡片上。按钮要 stopPropagation，否则会连带触发卡片自身的"进入"。 */}
                  <div className="yn-env-card__foot">
                    <span style={{ display: 'flex', gap: 12, minWidth: 0, overflow: 'hidden' }}>
                      <span style={{ whiteSpace: 'nowrap' }}>
                        入口域名 <span className="yn-mono">{env.ingressDomain || '—'}</span>
                      </span>
                      <span style={{ whiteSpace: 'nowrap' }}>负责人 {env.owner || '—'}</span>
                    </span>
                    <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
                      <Button
                        type="link"
                        size="small"
                        icon={<EditOutlined />}
                        style={{ paddingInline: 4, fontSize: 12, height: 20 }}
                        disabled={!canWrite}
                        onClick={(e) => {
                          e.stopPropagation();
                          openEdit(env);
                        }}
                      >
                        编辑
                      </Button>
                    </DisabledTip>
                  </div>
                </div>
              </Col>
            );
          })}
        </Row>
      ) : (
        <SectionCard bodyStyle={{ padding: 0 }}>
          <Table<Environment>
            rowKey="id"
            size="middle"
            loading={envApi.loading}
            dataSource={envs}
            columns={columns}
            scroll={{ x: 1720 }}
            pagination={{ pageSize: 12, showTotal: (t) => `共 ${t} 个环境` }}
          />
        </SectionCard>
      )}

      {/* -------------------------------------------------- 新建 / 编辑环境 */}
      <Drawer
        open={drawerOpen}
        width={720}
        title={editTarget ? `编辑环境 · ${editTarget.name}` : '新建环境'}
        onClose={() => setDrawerOpen(false)}
        extra={
          <Space>
            <Button onClick={() => setDrawerOpen(false)}>取消</Button>
            <Button type="primary" loading={saving} onClick={submit}>
              保存
            </Button>
          </Space>
        }
        destroyOnClose
      >
        <Form form={form} layout="vertical" requiredMark="optional">
          <Row gutter={16}>
            <Col span={12}>
              <Form.Item name="name" label="环境名称" rules={[{ required: true, message: '请输入环境名称' }]}>
                <Input placeholder="如：华东压测区" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item
                name="key"
                label="环境标识"
                rules={[
                  { required: true, message: '请输入环境标识' },
                  // 与后端 isValidKey **逐字一致**：必须以小写字母开头。
                  // 这里松一格（放行数字开头）的后果是"填得进去、提交必被 40000 拒"。
                  { pattern: /^[a-z][a-z0-9-]*$/, message: '小写字母开头，仅含小写字母、数字与中划线' },
                ]}
                tooltip="全局唯一，如 test / staging / prod。创建后不可修改。"
              >
                <Input placeholder="perf-sh" disabled={!!editTarget} />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item
                name="cluster"
                label="所属集群"
                rules={[{ required: true, message: '请选择或输入集群' }]}
                extra={
                  clusterOptions.length
                    ? '来自「系统管理 › 集群接入」的已接入集群'
                    : '平台还没有接入任何集群：去「系统管理 › 集群接入」导入 kubeconfig 后即可选'
                }
              >
                <AutoComplete
                  options={clusterOptions}
                  placeholder="选择已接入集群，或输入新标识"
                  filterOption={(input, option) => String(option?.value ?? '').includes(input)}
                />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="namespace" label="Namespace" rules={[{ required: true, message: '请输入 Namespace' }]}>
                <Input placeholder="如：yunnian-perf-sh" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="deployMethod" label="部署方式" rules={[{ required: true }]}>
                <Select
                  options={[
                    { label: 'K8s Deployment', value: 'k8s' },
                    { label: 'Helm', value: 'helm' },
                    { label: 'Docker Compose', value: 'compose' },
                    { label: '自定义脚本', value: 'script' },
                  ]}
                />
              </Form.Item>
            </Col>
            <Col span={6}>
              <Form.Item name="needApproval" label="部署审批" valuePropName="checked">
                <Switch checkedChildren="需要" unCheckedChildren="免审批" />
              </Form.Item>
            </Col>
            <Col span={6}>
              {/* 环境与集群「两者都允许」（用户 2026-09-29 定）：既可以引用平台已接入
                  集群的凭据，也可以自带一份（临时集群、别人托管的集群）。
                  自带时凭据按 current-context 最小化后单独加密，挂在
                  environment.kubeconfig_credential_id 上。 */}
              <Form.Item
                name="ownKubeconfig"
                label="独立 kubeconfig"
                valuePropName="checked"
                tooltip="打开后该环境使用自己那份 kubeconfig，而不是引用上面所选集群在平台登记的凭据"
              >
                <Switch checkedChildren="自带" unCheckedChildren="引用集群" />
              </Form.Item>
            </Col>
            {editTarget ? (
              <Col span={6}>
                <Form.Item name="enabled" label="启用状态" valuePropName="checked">
                  <Switch checkedChildren="启用" unCheckedChildren="停用" />
                </Form.Item>
              </Col>
            ) : null}

            {/* 审批人：只有开了「部署审批」才出现，但它是**必需**的 ——
                后端对 needApproval=true 且审批人为空的环境直接拒单
                （"需要审批的环境至少要指定一名审批人"）。缺了它，上面那个开关就是个死开关。 */}
            <Col span={24}>
              <Form.Item noStyle shouldUpdate={(p, c) => p.needApproval !== c.needApproval}>
                {({ getFieldValue }) =>
                  getFieldValue('needApproval') ? (
                    <Form.Item
                      name="approvers"
                      label="审批人"
                      rules={[{ required: true, message: '需要审批的环境至少指定一名审批人' }]}
                      tooltip="提交部署后由这些账号审批。审批人请用平台账号（用户名）。"
                    >
                      <Select mode="multiple" optionFilterProp="label" options={userOptions} placeholder="选择审批人" />
                    </Form.Item>
                  ) : null
                }
              </Form.Item>
            </Col>

            {ownKube ? (
              <Col span={24}>
                {editTarget?.hasKubeconfig ? (
                  <Alert
                    type="info"
                    showIcon
                    style={{ marginBottom: 12 }}
                    message="该环境已配置自带 kubeconfig（内容不会回显）。留空保存＝沿用原凭据；重新粘贴＝换一份。"
                  />
                ) : null}
                <Form.Item
                  name="kubeconfig"
                  label="该环境的 kubeconfig"
                  rules={editTarget ? [] : [{ required: true, message: '请粘贴或上传 kubeconfig 内容' }]}
                  extra="按 current-context 最小化后加密存储（AES-256-GCM）；保存后界面不再回显内容"
                >
                  <Input.TextArea
                    rows={4}
                    className="yn-mono"
                    style={{ fontSize: 12 }}
                    placeholder={'apiVersion: v1\nkind: Config\n...'}
                  />
                </Form.Item>
              </Col>
            ) : null}

            <Col span={12}>
              {/* 阶段由环境自带：自定义环境（如「华东压测区」）也能排进链路，
                  而不是因为 key 匹配不上默认映射被扔到最后 */}
              <Form.Item name="stageLabel" label="发布阶段" tooltip="拓扑链路里显示的阶段名，如「性能压测」">
                <Input placeholder="如：性能压测" maxLength={16} />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="stageOrder" label="链路序号" tooltip="越小越靠左（越接近研发）。留空则排在其他环境之后">
                <InputNumber min={1} max={99} style={{ width: '100%' }} placeholder="如：3" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="owner" label="负责人" tooltip="用于环境责任归属，可留空">
                <Select allowClear showSearch optionFilterProp="label" options={userOptions} placeholder="选择负责人" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="description" label="环境描述">
                <Input placeholder="说明该环境的用途与发布规范" maxLength={200} />
              </Form.Item>
            </Col>

            {/* —— 以下仅编辑时出现：都是"环境已经存在"才谈得上的配置 ——
                projectKeys 尤其不能省：纳管项目才会往 service_instances 里建行，
                而 POST /deployments 认的就是服务实例。新建时环境还没有 key，
                候选接口都调不通，所以那时显示它只会是个灰框。 */}
            {editTarget ? (
              <>
                <Col span={12}>
                  <Form.Item name="registry" label="镜像仓库地址" tooltip="留空则使用平台默认仓库">
                    <Input placeholder="harbor.yunnian.internal" />
                  </Form.Item>
                </Col>
                <Col span={12}>
                  <Form.Item name="ingressDomain" label="入口域名">
                    <Input placeholder="*.test.yunnian.cn" />
                  </Form.Item>
                </Col>
                <Col span={24}>
                  <Form.Item
                    name="projectKeys"
                    label="纳管项目"
                    tooltip="选中的项目会接入该环境（同时建出服务实例）。只有纳管之后，项目才能在这个环境里发布。"
                  >
                    <Select
                      mode="multiple"
                      optionFilterProp="label"
                      placeholder="选择项目"
                      loading={candApi.loading}
                      options={candidates.map((c) => ({ label: `${c.name}（${c.key}）`, value: c.key }))}
                    />
                  </Form.Item>
                </Col>
                <Col span={24}>
                  {/* 外层 Form.Item 不带 name：KVFields 内部已是 Form.List name="vars"，
                      同名套两层会让两个 field 争抢同一个数据路径。 */}
                  <Form.Item
                    label="环境变量 / 配置参数"
                    tooltip="对所有部署到本环境的服务生效。密文项在列表页不会回显明文。"
                  >
                    <KVFields name="vars" addText="添加环境变量" keyPlaceholder="如 DB_HOST" />
                  </Form.Item>
                </Col>
              </>
            ) : null}
          </Row>
        </Form>
      </Drawer>
    </div>
  );
};

export default EnvironmentList;
