import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  AutoComplete,
  Button,
  Col,
  Drawer,
  Form,
  Input,
  InputNumber,
  Row,
  Select,
  Space,
  Switch,
} from 'antd';
import { KVFields } from '@/components/KVFields';
import { clusterApi, environmentApi, userApi } from '@/api';
import type { EnvProjectCandidate } from '@/api/cd';
import { clusterBadgeOf, clusterKeyOf } from '@/config/topology';
import { useApi } from '@/hooks/useApi';
import type { Environment, EnvVar } from '@/types';

/**
 * 新建/编辑抽屉的表单值。与后端 `EnvironmentInput` 逐字段对齐
 * （本抽屉是环境的**唯一写入入口**，字段名对不上就会静默丢弃）。
 *
 * 「新建」只渲染建环境真正必需的字段；带 * 的几项依赖"环境已经存在"，
 * 仅在编辑时出现 —— 最典型的是 `projectKeys`：纳管项目要往 `service_instances`
 * 里建行，而那个表的外键指向环境，环境还没建出来就无从谈起。
 */
export interface EnvFormValues {
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

export interface EnvironmentEditDrawerProps {
  open: boolean;
  /** 编辑目标；`null` = 新建。 */
  target: Environment | null;
  /**
   * **现有环境用过的集群标识**，用来补"存量候选"。
   *
   * 集群实体化之前 `environment.cluster` 是自由文本，历史值不在集群表里；
   * 编辑时若不把它们列出来，用户连"当前所属集群"都看不到（保存一次就会被改成空）。
   * 调用方请传 `useMemo` 过的数组，否则每次渲染都会让候选重算。
   */
  clusterValuesInUse?: string[];
  onClose: () => void;
  /** 保存成功后回调（列表页刷新列表 / 详情页刷新环境）。 */
  onSaved: () => void;
}

/**
 * 环境的**新建 / 编辑**抽屉。列表页与详情页共用同一份实现。
 *
 * 为什么提出来：这个抽屉原先只长在 `EnvironmentList` 里，于是"环境详情页没有
 * 环境级编辑入口"——从卡片点进去之后反而什么都改不了。而**再抄一份表单**是更糟的
 * 选择：`EnvironmentInput` 的字段一旦增减，两份必然漂移，漂移的后果是**静默丢字段**
 * （`UpdateEnvironment` 对多数字段是**无条件覆盖**写回，漏传哪个就清空哪个）。
 * 所以集群候选、用户候选、变量回填、提交口径全部收在这里，调用方只管开/关。
 */
export const EnvironmentEditDrawer: React.FC<EnvironmentEditDrawerProps> = ({
  open,
  target,
  clusterValuesInUse,
  onClose,
  onSaved,
}) => {
  const { message } = AntApp.useApp();
  const [saving, setSaving] = useState(false);
  const [form] = Form.useForm<EnvFormValues>();
  // 「独立 kubeconfig」开关：打开后才渲染凭据输入框。
  // 用 useWatch 取值而不是拿 shouldUpdate 包一层，避免整块 Row 在每次击键时重渲染。
  const ownKube = Form.useWatch('ownKubeconfig', form) as boolean | undefined;

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
    const legacy = Array.from(new Set((clusterValuesInUse ?? []).map(clusterKeyOf).filter(Boolean)))
      .filter((c) => !known.has(c))
      .sort()
      .map((c) => ({ label: `${c}${suffix}`, value: c }));
    return [...fromPlatform, ...legacy];
  }, [clustersApi.data, clustersApi.error, clusterValuesInUse]);

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
   * `EnvironmentDTO.owner` 回的是**展示名**，而选择器的 `value` 是**用户名** ——
   * 两个标识对不上，预填过去的下拉里根本没有这一项，于是 AntD 只能把原值当标签渲染
   * （用户看到"运维示例"而不是"运维示例（ops）"，打开下拉也不会被选中）。
   * 审批人走的是后端专门回的 `approverUsernames`，负责人没有对应字段，所以在这里换一次。
   * 换不到就原样保留（值本身可能就是用户名，或者用户列表还没回来）—— 后端
   * `resolveOwner` 按 `name = ? OR username = ?` 查，两种写法都能存。
   */
  const usernameByName = useMemo(() => {
    const m = new Map<string, string>();
    (usersApi.data ?? []).forEach((u) => m.set(u.name, u.username));
    return m;
  }, [usersApi.data]);

  // 编辑时要回填环境变量（它不在环境对象上，得单独取 /environments/{key}/vars）
  const varsApi = useApi(() => environmentApi.vars(target!.key), [target?.key], {
    enabled: !!target,
  });

  // 编辑时的项目候选（新建时还没有 key，无法调候选接口）
  const candApi = useApi(() => environmentApi.projectCandidates(target!.key), [target?.key], {
    enabled: !!target,
  });
  const candidates: EnvProjectCandidate[] = useMemo(() => candApi.data ?? [], [candApi.data]);

  /**
   * 打开时整体覆写表单。
   *
   * 放在抽屉内部而不是让调用方在 `setOpen(true)` 之前 `setFieldsValue`：
   * 两个调用方（列表页、详情页）否则都得记得先填再开，而**漏填的后果不是空的**——
   * 是"看起来正常、一保存就把某个字段清掉"。回填口径只留这一处。
   */
  useEffect(() => {
    if (!open) return;
    if (!target) {
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
      return;
    }
    form.setFieldsValue({
      name: target.name,
      key: target.key,
      cluster: target.cluster,
      clusterVersion: target.clusterVersion,
      namespace: target.namespace,
      deployMethod: target.deployMethod,
      registry: target.registry,
      ingressDomain: target.ingressDomain,
      // 负责人：DTO 回的是**展示名**，而选择器的 value 是用户名 → 换一次再预填，
      // 否则这个值在下拉的选项里不存在（见 `usernameByName`）。
      owner: usernameByName.get(target.owner) ?? target.owner,
      needApproval: target.needApproval,
      // 审批人用**用户名**预填（`approverUsernames` 与 `approvers` 一一对应，是后端
      // 专门为这个表单回的）。以前只回展示名、这里只能留空让人重选 ——
      // 于是"打开编辑不重选"就等于把审批人清空，needApproval 时更会被下面的必填规则
      // 拦住、逼用户每次重新点一遍。
      approvers: target.approverUsernames ?? [],
      projectKeys: target.projectKeys,
      enabled: target.enabled,
      description: target.description,
      stageLabel: target.stageLabel,
      stageOrder: target.stageOrder,
      // 凭据只回「有没有配」：开关反映现状，但**不回填内容**（明文没有读出口）。
      // 用户不重新粘贴 ⇒ 提交时 kubeconfig 为空 ⇒ 后端按「不修改凭据」处理。
      ownKubeconfig: target.clusterSource === 'own' || !!target.hasKubeconfig,
      kubeconfig: '',
      vars: [],
    });
    // target 只在"换成另一个环境"时需要重填；带上 open 是为了"关掉再开同一个环境"
    // 也能把上一次没保存的改动丢掉。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, target?.key]);

  // 变量取回后补进表单（不能放在上面那个回填里——那边拿不到异步结果）。
  // `open` 必须在依赖里：关掉再打开同一个环境时 `varsApi.data` 没变，
  // 只靠它不会重跑，而上面刚把 `vars` 清成了 `[]`，变量区就空了。
  useEffect(() => {
    if (!open || !target || !varsApi.data) return;
    form.setFieldsValue({ vars: varsApi.data });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [varsApi.data, open, target?.key]);

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
        approvers: v.approvers ?? target?.approverUsernames ?? [],
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

      const saved = target
        ? await environmentApi.update(target.key, body)
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

      message.success(target ? `环境「${v.name}」已保存` : `环境「${v.name}」已创建`);
      onClose();
      onSaved();
    } finally {
      setSaving(false);
    }
  };

  return (
    <Drawer
      open={open}
      width={720}
      title={target ? `编辑环境 · ${target.name}` : '新建环境'}
      onClose={onClose}
      extra={
        <Space>
          <Button onClick={onClose}>取消</Button>
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
              <Input placeholder="perf-sh" disabled={!!target} />
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
          {target ? (
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
              {target?.hasKubeconfig ? (
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
                rules={target ? [] : [{ required: true, message: '请粘贴或上传 kubeconfig 内容' }]}
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
          {target ? (
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
  );
};

export default EnvironmentEditDrawer;
