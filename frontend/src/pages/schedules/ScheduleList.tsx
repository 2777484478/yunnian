import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Form,
  Input,
  Modal,
  Popconfirm,
  Segmented,
  Select,
  Space,
  Spin,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import {
  ClockCircleOutlined,
  CloudServerOutlined,
  DeleteOutlined,
  EditOutlined,
  MinusCircleOutlined,
  PlayCircleOutlined,
  PlusOutlined,
  ReloadOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link } from 'react-router-dom';
import { PageHeader, SectionCard } from '@/components/ui';
import { environmentApi } from '@/api/cd';
import { imageApi, projectApi } from '@/api/ci';
import { isGitParam, jenkinsApi } from '@/api';
import { scheduleApi } from '@/api/schedule';
import type { ScheduleInput, ScheduleTarget, ScheduleTask, ScheduleTaskType } from '@/api/schedule';
import type { Project } from '@/types';
import { useApi } from '@/hooks/useApi';
import { hasPerm } from '@/auth';
import { fmt } from '@/utils/format';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 定时任务
//
// 定位：**到点替用户"点一下按钮"**，它不自己跑构建 / 部署 —— 只建单，
// 排队、并发、失败通知、超时判定全部复用现成链路（见后端 service/schedule.go）。
//
// 这一版有三件事是用户明确提的（2026-10-29）：
//
//   1. "定时构建肯定要填写项目的分支啊，不用默认参数，手动可选" ——
//      分支不再是"留空用默认"，而是每行一个下拉：
//        · 平台自建构建 → 从**仓库真实分支**里选（也能手填）；
//        · Jenkins 项目   → 从**该 Job 的 git 参数候选值**里选，并记住参数名。
//      为什么必须显式指定：定时任务半夜跑，分支若靠项目默认值，
//      默认分支一改，第二天构建的就不是你排的那条，而且**不会报错**。
//
//   2. "定时任务为什么只能选一个项目？部署发布的时候怎么可能只有 1 个服务？"
//      → 一条任务带多个目标（批量），逐条独立建单，一条失败不影响其余。
//
//   3. "定时任务菜单放在镜像构建下面不合适，单独放" → 顶层独立菜单。
//
// 渲染上的两条纪律（都是踩过的）：
//   · 分支 / 版本候选**按行懒加载**（点开下拉才去读仓库 / Jenkins）。
//     一次批量 15 个服务时全量预取 = 15 次 git 探测 + 15 次 Jenkins 调用，
//     而用户很可能只改其中一行。
//   · 读不到候选值要说原因，并**仍然允许手填** —— 仓库凭据临时失效时
//     也不该把人卡死在这里（后端只认最终的分支字符串）。
// ---------------------------------------------------------------------------

/**
 * cron → 人话。
 *
 * 为什么要有它：`0 2 * * *` 对写惯 cron 的人很清楚，但对"我只想每天两点跑一次"
 * 的人来说是一串符号。列表里 8 个任务全是表达式，等于每次都要在脑子里翻译一遍。
 *
 * 只覆盖最常见的那几种；认不出来就原样显示表达式 —— **绝不猜**，
 * 猜错会让人以为任务的时间和实际不符。
 */
function describeCron(expr: string): string {
  const p = expr.trim().split(/\s+/);
  if (p.length !== 5) return '';
  const [mi, ho, dom, mon, dow] = p;
  const isNum = (s: string) => /^\d+$/.test(s);

  // 每 N 分钟
  if (p[0].startsWith('*/') && p[1] === '*' && dom === '*' && mon === '*' && dow === '*') {
    return `每 ${p[0].slice(2)} 分钟`;
  }
  // 每小时第 M 分
  if (isNum(mi) && ho === '*' && dom === '*' && mon === '*' && dow === '*') {
    return `每小时第 ${mi} 分`;
  }
  // 每 N 小时
  if (isNum(mi) && ho.startsWith('*/') && dom === '*' && mon === '*' && dow === '*') {
    return `每 ${ho.slice(2)} 小时（第 ${mi} 分）`;
  }
  if (!isNum(mi) || !isNum(ho)) return '';
  const hhmm = `${ho.padStart(2, '0')}:${mi.padStart(2, '0')}`;

  // 每天
  if (dom === '*' && mon === '*' && dow === '*') return `每天 ${hhmm}`;
  // 工作日
  if (dom === '*' && mon === '*' && dow === '1-5') return `工作日 ${hhmm}`;
  // 每周几
  if (dom === '*' && mon === '*' && isNum(dow)) {
    const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
    return `每${names[Number(dow)] ?? `周${dow}`} ${hhmm}`;
  }
  // 每周几（多选）
  if (dom === '*' && mon === '*' && /^[\d,]+$/.test(dow)) {
    const names = ['日', '一', '二', '三', '四', '五', '六'];
    const ds = dow.split(',').map((d) => names[Number(d)] ?? d);
    return `每周${ds.join('、')} ${hhmm}`;
  }
  // 每月某天
  if (isNum(dom) && mon === '*' && dow === '*') return `每月 ${dom} 号 ${hhmm}`;
  return '';
}

/** 常用预设，用户不用会写 cron。 */
const CRON_PRESETS: { label: string; cron: string }[] = [
  { label: '每天 02:00', cron: '0 2 * * *' },
  { label: '每天 06:00', cron: '0 6 * * *' },
  { label: '每工作日 09:00', cron: '0 9 * * 1-5' },
  { label: '每小时', cron: '0 * * * *' },
  { label: '每 30 分钟', cron: '*/30 * * * *' },
];

/** 上次执行结果 → 标签。 */
function LastStatusTag({ task }: { task: ScheduleTask }) {
  const st = task.lastStatus;
  if (!st) return <Text type="secondary">还没执行过</Text>;
  const map: Record<string, { color: string; label: string }> = {
    success: { color: 'green', label: '成功' },
    // partial 是批量特有的：一部分目标成功、一部分失败。
    // 不能并进"成功"（会让人以为全发完了），也不能并进"失败"
    // （明明已经发上去一部分，重试前得先知道是哪些）。
    partial: { color: 'orange', label: '部分成功' },
    failed: { color: 'red', label: '失败' },
    skipped: { color: 'orange', label: '跳过' },
    missed: { color: 'gold', label: '错过' },
    running: { color: 'blue', label: '执行中' },
  };
  const m = map[st] ?? { color: 'default', label: st };
  return (
    <Space size={6} direction="vertical" style={{ lineHeight: 1.4 }}>
      <Space size={6}>
        <Tag color={m.color} style={{ marginInlineEnd: 0 }}>
          {m.label}
        </Tag>
        {task.lastRunAt ? (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {fmt(task.lastRunAt)}
          </Text>
        ) : null}
      </Space>
      {task.lastError ? (
        <Tooltip title={task.lastError}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {task.lastError.length > 34 ? `${task.lastError.slice(0, 34)}…` : task.lastError}
          </Text>
        </Tooltip>
      ) : null}
      {task.missedCount > 0 ? (
        <Tooltip title="服务停机期间到点但没执行。按配置不会补跑">
          <Text type="warning" style={{ fontSize: 12 }}>
            累计错过 {task.missedCount} 次
          </Text>
        </Tooltip>
      ) : null}
    </Space>
  );
}

// --------------------------------------------------------------------------- 目标编辑器

/**
 * 编辑态里的一行目标。
 *
 * `imageChoice` 用带前缀的字符串统一两种版本来源（`p:<id>` 平台构建 / `r:<tag>` 仓库标签），
 * 与批量发布同一套口径 —— 提交时按前缀分流，**不能两个都给**。
 */
interface TargetRowState {
  projectKey: string;
  branch: string;
  branchParam: string;
  imageChoice: string;
}

/** Jenkins 分支参数的探测状态。 */
type GitParamState = 'unknown' | 'none' | 'ok';

interface TargetRowProps {
  row: TargetRowState;
  kind: ScheduleTaskType;
  /** deploy 且"指定固定版本"时每行要选版本 */
  needVersion: boolean;
  project?: Project;
  onChange: (patch: Partial<TargetRowState>) => void;
  onRemove: () => void;
}

const TargetRowEditor: React.FC<TargetRowProps> = ({
  row,
  kind,
  needVersion,
  project,
  onChange,
  onRemove,
}) => {
  const [gitState, setGitState] = useState<GitParamState>('unknown');
  const [branches, setBranches] = useState<string[]>([]);
  const [tags, setTags] = useState<string[]>([]);
  const [branchLoading, setBranchLoading] = useState(false);
  const [branchLoaded, setBranchLoaded] = useState(false);
  const [branchErr, setBranchErr] = useState<string | null>(null);
  const [search, setSearch] = useState('');

  const [imgOpts, setImgOpts] = useState<{ label: string; value: string }[]>([]);
  const [imgLoading, setImgLoading] = useState(false);
  const [imgLoaded, setImgLoaded] = useState(false);
  const [imgErr, setImgErr] = useState<string | null>(null);

  /**
   * 读分支候选。
   *
   * 两条完全不同的来源，别混：
   *   · 平台自建构建 → 项目仓库的分支（真实 git ls-remote）；
   *   · Jenkins 项目 → 该 Job 的 **git 参数**候选值，并顺带定下"传给哪个参数名"。
   *     Jenkins 的默认分支平台读不到（插件不通过 API 暴露 defaultValue），
   *     所以以前那种"留空用默认"在定时任务里等于把版本交给运气。
   */
  const loadBranches = async () => {
    if (!project) return;
    setBranchLoading(true);
    setBranchErr(null);
    try {
      if (project.buildMethod === 'jenkins') {
        const job = (project.jenkinsJob ?? '').trim();
        if (!job) {
          setGitState('none');
          setBranchErr('这个项目还没绑定 Jenkins Job，请先到「项目管理」里选好 Job');
          return;
        }
        const defs = await jenkinsApi.params(job);
        const gitDefs = defs.filter((d) => isGitParam(d.kind));
        if (!gitDefs.length) {
          setGitState('none');
          setBranches([]);
          setTags([]);
          onChange({ branchParam: '' });
          return;
        }
        // 一个 Job 有多个 git 参数时，挑名字里带 branch 的那个；
        // 都没有就取第一个 —— 并把这个参数名写进目标里，执行时不必再问 Jenkins。
        const pick = gitDefs.find((d) => /branch/i.test(d.name)) ?? gitDefs[0];
        const v = await jenkinsApi.paramValues(job, pick.name);
        setGitState('ok');
        setBranches(v.branches ?? []);
        setTags(v.tags ?? []);
        if (v.errors?.length) setBranchErr(v.errors.join('；'));
        if (row.branchParam !== pick.name) onChange({ branchParam: pick.name });
      } else {
        const list = await projectApi.branches(project.key);
        setGitState('ok');
        setBranches(list ?? []);
        setTags([]);
      }
    } catch (e) {
      setBranchErr((e as Error)?.message ?? '读取分支失败');
    } finally {
      setBranchLoading(false);
      setBranchLoaded(true);
    }
  };

  const loadImages = async () => {
    if (!row.projectKey) return;
    setImgLoading(true);
    setImgErr(null);
    try {
      const res = await imageApi.list({ projectKey: row.projectKey, pageSize: 50 });
      setImgOpts((res.list ?? []).map((im) => ({ value: `p:${im.id}`, label: `${im.tag}（平台构建）` })));
    } catch (e) {
      setImgErr((e as Error)?.message ?? '读取版本失败');
    } finally {
      setImgLoading(false);
      setImgLoaded(true);
    }
  };

  // 候选值只在点开下拉时才拉（理由见文件头）
  const ensureBranches = () => {
    if (!branchLoaded && !branchLoading) void loadBranches();
  };
  const ensureImages = () => {
    if (!imgLoaded && !imgLoading) void loadImages();
  };
  // 编辑已有任务时，分支是必填项：没候选也要能显示当前值，
  // 所以这里不预取；但一旦用户点开下拉就会补上候选。

  const branchOptions = useMemo(() => {
    const groups: { label: string; options: { label: string; value: string }[] }[] = [];
    if (branches.length) {
      groups.push({
        label: `分支 ${branches.length}`,
        options: branches.map((x) => ({ label: x, value: x })),
      });
    }
    if (tags.length) {
      groups.push({
        label: `标签 ${tags.length}`,
        options: tags.map((x) => ({ label: x, value: x })),
      });
    }
    const q = search.trim();
    if (q && !branches.includes(q) && !tags.includes(q)) {
      // 手动输入：仓库凭据临时失效、或分支刚推上去还没被探测到时，
      // 不能把人卡死在这里 —— 后端只认最终那个字符串。
      groups.push({ label: '手动输入', options: [{ label: q, value: q }] });
    }
    return groups;
  }, [branches, tags, search]);

  const isJenkins = project?.buildMethod === 'jenkins';
  const noGitParam = isJenkins && gitState === 'none';

  return (
    <div
      style={{
        display: 'flex',
        gap: 10,
        alignItems: 'flex-start',
        padding: '10px 12px',
        borderTop: '1px solid var(--yn-border)',
      }}
    >
      <div style={{ width: 220, flex: '0 0 220px', paddingTop: 6 }}>
        <div style={{ lineHeight: 1.4 }}>
          <b className="yn-mono" style={{ fontSize: 13 }}>
            {row.projectKey}
          </b>
        </div>
        <Text type="secondary" style={{ fontSize: 11.5 }}>
          {project?.name ?? ''}
          {isJenkins ? ' · Jenkins' : ''}
        </Text>
      </div>

      <div style={{ flex: '1 1 auto', minWidth: 0 }}>
        {kind === 'build' ? (
          <>
            <Select
              showSearch
              allowClear
              style={{ width: '100%' }}
              value={row.branch || undefined}
              placeholder={noGitParam ? '该 Job 没有分支参数' : '选择分支（可直接输入）'}
              disabled={noGitParam}
              loading={branchLoading}
              onOpenChange={(open) => {
                if (open) ensureBranches();
              }}
              onSearch={setSearch}
              onChange={(v) => onChange({ branch: (v as string) ?? '' })}
              options={branchOptions}
              optionFilterProp="label"
              listHeight={260}
              notFoundContent={
                branchLoading ? (
                  <div style={{ padding: 10, textAlign: 'center' }}>
                    <Spin size="small" />
                  </div>
                ) : (
                  <div style={{ padding: 10 }}>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {branchErr ? '没读到候选值，可直接输入分支名' : '没有候选值，可直接输入分支名'}
                    </Text>
                  </div>
                )
              }
            />
            <div style={{ marginTop: 4, fontSize: 11.5, lineHeight: 1.6 }}>
              {branchErr ? (
                <Text type="warning" style={{ fontSize: 11.5 }}>
                  读取候选值失败：{branchErr}（仍可手动输入）
                </Text>
              ) : isJenkins ? (
                <Text type="secondary" style={{ fontSize: 11.5 }}>
                  {noGitParam
                    ? '这个 Job 没有 git 参数，分支只能用 Job 自己配的值'
                    : `取自 Job 的 git 参数${row.branchParam ? ` ${row.branchParam}` : ''}；不选则由 Jenkins 用它自己的默认值`}
                </Text>
              ) : (
                <Text type="secondary" style={{ fontSize: 11.5 }}>
                  取自仓库真实分支；到点就用这个分支构建
                </Text>
              )}
            </div>
          </>
        ) : needVersion ? (
          <>
            <Select
              showSearch
              allowClear
              style={{ width: '100%' }}
              value={row.imageChoice || undefined}
              placeholder="选择要发的版本"
              loading={imgLoading}
              onOpenChange={(open) => {
                if (open) ensureImages();
              }}
              onChange={(v) => onChange({ imageChoice: (v as string) ?? '' })}
              options={imgOpts}
              optionFilterProp="label"
              listHeight={260}
              notFoundContent={
                imgLoading ? (
                  <div style={{ padding: 10, textAlign: 'center' }}>
                    <Spin size="small" />
                  </div>
                ) : (
                  <div style={{ padding: 10 }}>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      这个项目在平台上还没有构建记录
                    </Text>
                  </div>
                )
              }
            />
            {imgErr ? (
              <div style={{ marginTop: 4 }}>
                <Text type="warning" style={{ fontSize: 11.5 }}>
                  {imgErr}
                </Text>
              </div>
            ) : null}
          </>
        ) : (
          <Text type="secondary" style={{ fontSize: 12, lineHeight: '30px' }}>
            每次执行时取该项目<b>此刻最新</b>的构建
          </Text>
        )}
      </div>

      <Tooltip title="从这批目标里去掉">
        <Button
          type="text"
          size="small"
          danger
          icon={<MinusCircleOutlined />}
          style={{ marginTop: 3 }}
          onClick={onRemove}
        />
      </Tooltip>
    </div>
  );
};

// --------------------------------------------------------------------------- 页面

const ScheduleList: React.FC = () => {
  const { message } = AntApp.useApp();
  const canWrite = hasPerm('build:trigger') || hasPerm('deploy:read');

  const [taskType, setTaskType] = useState<string>('');
  const listApi = useApi((signal) => scheduleApi.list(), []);
  const rows = listApi.data ?? [];

  const projApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const envApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);
  const projects = projApi.data?.list ?? [];
  const envs = envApi.data?.list ?? [];

  const shown = useMemo(
    () => (taskType ? rows.filter((r) => r.taskType === taskType) : rows),
    [rows, taskType],
  );

  // ---- 弹窗 ----
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<ScheduleTask | null>(null);
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState<string | null>(null);

  const [name, setName] = useState('');
  const [kind, setKind] = useState<ScheduleTaskType>('build');
  const [envKey, setEnvKey] = useState('');
  const [cron, setCron] = useState('0 2 * * *');
  const [targets, setTargets] = useState<TargetRowState[]>([]);
  const [imageSource, setImageSource] = useState<'fixed' | 'latest'>('latest');
  const [remark, setRemark] = useState('');
  const [enabled, setEnabled] = useState(true);

  // cron 预览（防抖）
  const [preview, setPreview] = useState<{ valid: boolean; error: string; next: string[] } | null>(null);
  const previewTimer = useRef<number | null>(null);

  const projByKey = useMemo(() => {
    const m = new Map<string, Project>();
    for (const p of projects) m.set(p.key, p);
    return m;
  }, [projects]);

  useEffect(() => {
    if (!open) return;
    if (previewTimer.current) window.clearTimeout(previewTimer.current);
    previewTimer.current = window.setTimeout(() => {
      void scheduleApi
        .preview(cron)
        .then((r) => setPreview(r))
        .catch(() => setPreview(null));
    }, 300);
    return () => {
      if (previewTimer.current) window.clearTimeout(previewTimer.current);
    };
  }, [cron, open]);

  const openNew = () => {
    setEditing(null);
    setName('');
    setKind('build');
    setEnvKey('');
    setCron('0 2 * * *');
    setTargets([]);
    setImageSource('latest');
    setRemark('');
    setEnabled(true);
    setPreview(null);
    setOpen(true);
  };

  const openEdit = (t: ScheduleTask) => {
    setEditing(t);
    setName(t.name);
    setKind(t.taskType);
    setEnvKey(t.envKey);
    setCron(t.cron);
    // 目标：优先用 targets 列；旧数据（后端已按镜像列还原）也走这里
    const list = t.targets?.length
      ? t.targets
      : [{ projectKey: t.projectKey, branch: t.branch, imageTag: t.imageTag, imageRecordId: t.imageRecordId }];
    setTargets(
      list.map((x: ScheduleTarget) => ({
        projectKey: x.projectKey,
        branch: x.branch ?? '',
        branchParam: x.branchParam ?? '',
        imageChoice: x.imageRecordId ? `p:${x.imageRecordId}` : x.imageTag ? `r:${x.imageTag}` : '',
      })),
    );
    setImageSource((t.imageSource as 'fixed' | 'latest') || 'latest');
    setRemark(t.remark ?? '');
    setEnabled(t.enabled);
    setPreview(null);
    setOpen(true);
  };

  /** 项目多选 → 同步行。已选中的行保留它填过的分支 / 版本（不能因为加一行就全清）。 */
  const setProjectKeys = (keys: string[]) => {
    setTargets((prev) => {
      const byKey = new Map(prev.map((r) => [r.projectKey, r]));
      return keys.map(
        (k) => byKey.get(k) ?? { projectKey: k, branch: '', branchParam: '', imageChoice: '' },
      );
    });
  };

  const patchRow = (key: string, patch: Partial<TargetRowState>) => {
    setTargets((prev) => prev.map((r) => (r.projectKey === key ? { ...r, ...patch } : r)));
  };

  const save = async () => {
    if (!name.trim()) {
      message.warning('给任务起个名字：它会显示在构建/部署记录的"操作人"上');
      return;
    }
    if (!targets.length) {
      message.warning('至少选一个项目');
      return;
    }
    if (kind === 'deploy' && !envKey) {
      message.warning('定时部署必须选择环境');
      return;
    }
    if (!cron.trim()) {
      message.warning('请填 cron 表达式');
      return;
    }
    if (kind === 'build') {
      // 分支必须逐个检查（后端也校验，但在这里拦下来能直接指出是哪一行）
      for (const r of targets) {
        const p = projByKey.get(r.projectKey);
        const jenkins = p?.buildMethod === 'jenkins';
        if (!r.branch.trim()) {
          // Jenkins 项目没有 git 参数时确实无从指定，只能交给 Job 自己
          if (jenkins && !r.branchParam) continue;
          message.warning(`项目 ${r.projectKey} 还没选分支：定时构建必须写明用哪个分支`);
          return;
        }
      }
    }
    if (kind === 'deploy' && imageSource === 'fixed') {
      for (const r of targets) {
        if (!r.imageChoice) {
          message.warning(`项目 ${r.projectKey} 还没选要发的版本`);
          return;
        }
      }
    }

    const body: ScheduleInput = {
      name: name.trim(),
      taskType: kind,
      cron: cron.trim(),
      enabled,
      remark: remark.trim(),
      targets: targets.map((r) => {
        const t: ScheduleTarget = { projectKey: r.projectKey };
        if (kind === 'build') {
          if (r.branch.trim()) t.branch = r.branch.trim();
          if (r.branchParam) t.branchParam = r.branchParam;
        } else if (imageSource === 'fixed' && r.imageChoice) {
          if (r.imageChoice.startsWith('p:')) t.imageRecordId = r.imageChoice.slice(2);
          else t.imageTag = r.imageChoice.slice(2);
        }
        return t;
      }),
    };
    if (kind === 'deploy') {
      body.envKey = envKey;
      body.imageSource = imageSource;
    }

    setSaving(true);
    try {
      if (editing) {
        await scheduleApi.update(editing.id, body);
        message.success('已保存');
      } else {
        await scheduleApi.create(body);
        message.success(`已创建（${targets.length} 个目标）`);
      }
      setOpen(false);
      listApi.refresh();
    } catch (e) {
      // 后端的拒绝理由都是能照着改的（cron 哪里非法、哪个环境没权限），原样展示
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const toggle = async (t: ScheduleTask) => {
    try {
      await scheduleApi.setEnabled(t.id, !t.enabled);
      message.success(t.enabled ? '已停用（不会再自动执行）' : '已启用');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '操作失败');
    }
  };

  const runNow = async (t: ScheduleTask) => {
    setRunning(t.id);
    try {
      await scheduleApi.runNow(t.id);
      message.success('已触发一次（不会改动下次计划时间）');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '触发失败');
    } finally {
      setRunning(null);
    }
  };

  const remove = async (t: ScheduleTask) => {
    try {
      await scheduleApi.remove(t.id);
      message.success('已删除');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '删除失败');
    }
  };

  const projectOptions = useMemo(
    () =>
      projects.map((p) => ({
        value: p.key,
        label: `${p.name}（${p.key}）${p.buildMethod === 'jenkins' ? ' · Jenkins' : ''}`,
      })),
    [projects],
  );

  const columns = [
    {
      title: '任务',
      dataIndex: 'name',
      width: 220,
      render: (_: unknown, r: ScheduleTask) => (
        <Space size={6} direction="vertical" style={{ lineHeight: 1.4 }}>
          <Space size={6}>
            <b>{r.name}</b>
            {!r.enabled ? <Tag>已停用</Tag> : null}
          </Space>
          {r.remark ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {r.remark}
            </Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: '类型',
      dataIndex: 'taskType',
      width: 92,
      render: (v: string) =>
        v === 'build' ? (
          <Tag color="blue" icon={<ThunderboltOutlined />}>
            构建
          </Tag>
        ) : (
          <Tag color="geekblue" icon={<CloudServerOutlined />}>
            部署
          </Tag>
        ),
    },
    {
      title: '目标',
      width: 300,
      render: (_: unknown, r: ScheduleTask) => {
        const list = r.targets ?? [];
        const n = list.length;
        const desc = (t: ScheduleTarget) => {
          if (r.taskType === 'build') return t.branch ? `分支 ${t.branch}` : 'Job 默认分支';
          if (r.imageSource === 'latest') return '取最新构建';
          return t.imageTag || (t.imageRecordId ? `#${t.imageRecordId}` : '未指定版本');
        };
        return (
          <Space size={4} direction="vertical" style={{ lineHeight: 1.4 }}>
            <Space size={6} wrap>
              <Tag style={{ marginInlineEnd: 0 }}>{n > 1 ? `${n} 个服务` : '1 个服务'}</Tag>
              {r.taskType === 'deploy' ? (
                <Text type="secondary" style={{ fontSize: 12 }}>
                  → {r.envKey}
                </Text>
              ) : null}
            </Space>
            <Tooltip
              title={
                <div style={{ lineHeight: 1.9 }}>
                  {list.map((t) => (
                    <div key={t.projectKey}>
                      {t.projectKey} · {desc(t)}
                    </div>
                  ))}
                </div>
              }
            >
              <Text type="secondary" className="yn-mono" style={{ fontSize: 12 }}>
                {list.slice(0, 2).map((t) => t.projectKey).join('、')}
                {n > 2 ? ` 等 ${n} 个` : ''}
              </Text>
            </Tooltip>
          </Space>
        );
      },
    },
    {
      title: '计划',
      dataIndex: 'cron',
      width: 180,
      render: (v: string) => {
        const human = describeCron(v);
        return (
          <Space size={4} direction="vertical" style={{ lineHeight: 1.4 }}>
            <span>{human || v}</span>
            {human ? (
              <Text type="secondary" className="yn-mono" style={{ fontSize: 12 }}>
                {v}
              </Text>
            ) : null}
          </Space>
        );
      },
    },
    {
      title: '下次执行',
      dataIndex: 'nextRunAt',
      width: 150,
      render: (v: string | null, r: ScheduleTask) =>
        !r.enabled ? (
          <Text type="secondary">已停用</Text>
        ) : v ? (
          // 同样去秒：表格里 19 个字符刚好把这一列撑满，秒对"下次什么时候跑"毫无信息量
          fmt(v).slice(0, 16)
        ) : (
          <Text type="secondary">—</Text>
        ),
    },
    {
      title: '上次结果',
      width: 240,
      render: (_: unknown, r: ScheduleTask) => (
        <Space size={8} align="start">
          <LastStatusTag task={r} />
          {r.lastBuildId ? (
            <Link to={`/ci/builds?keyword=${r.projectKey}`} style={{ fontSize: 12 }}>
              看构建
            </Link>
          ) : null}
          {r.lastDeployId ? (
            <Link to="/cd/deploys" style={{ fontSize: 12 }}>
              看部署
            </Link>
          ) : null}
        </Space>
      ),
    },
    {
      title: '操作',
      width: 160,
      fixed: 'right' as const,
      render: (_: unknown, r: ScheduleTask) =>
        canWrite ? (
          <Space size={2}>
            <Tooltip title="立即执行一次（不改动下次计划时间）">
              <Button
                type="text"
                size="small"
                icon={<PlayCircleOutlined />}
                loading={running === r.id}
                onClick={() => void runNow(r)}
              />
            </Tooltip>
            <Tooltip title={r.enabled ? '停用（不再自动执行）' : '启用'}>
              <Switch size="small" checked={r.enabled} onChange={() => void toggle(r)} />
            </Tooltip>
            <Tooltip title="编辑">
              <Button type="text" size="small" icon={<EditOutlined />} onClick={() => openEdit(r)} />
            </Tooltip>
            <Popconfirm
              title="删除这个定时任务？"
              description={<span>只删任务本身，已经发出去的构建/部署记录不受影响。</span>}
              okText="删除"
              okButtonProps={{ danger: true }}
              onConfirm={() => void remove(r)}
            >
              <Button type="text" size="small" danger icon={<DeleteOutlined />} />
            </Popconfirm>
          </Space>
        ) : (
          <Text type="secondary">—</Text>
        ),
    },
  ];

  const selectedKeys = targets.map((t) => t.projectKey);

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '定时任务' }]}
        title="定时任务"
        description="到点自动触发构建或部署。一条任务可以对多个项目 / 服务批量执行；它只负责「建单」——排队、并发、失败通知都走现有链路，与手动点击完全一致。"
      />

      <SectionCard bodyStyle={{ padding: 0 }}>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 12,
            padding: '12px 16px',
            borderBottom: '1px solid var(--yn-border)',
            flexWrap: 'wrap',
          }}
        >
          <Segmented
            value={taskType}
            onChange={(v) => setTaskType(String(v))}
            options={[
              { label: '全部', value: '' },
              { label: '定时构建', value: 'build' },
              { label: '定时部署', value: 'deploy' },
            ]}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            共 {shown.length} 个任务
          </Text>
          <div style={{ flex: '1 1 auto' }} />
          <Tooltip title="刷新">
            <Button icon={<ReloadOutlined />} onClick={listApi.refresh} />
          </Tooltip>
          {canWrite ? (
            <Button type="primary" icon={<PlusOutlined />} onClick={openNew}>
              新增任务
            </Button>
          ) : null}
        </div>

        {listApi.error ? (
          <div style={{ padding: 16 }}>
            <Alert type="error" showIcon message={listApi.error.message} />
          </div>
        ) : null}

        <Table<ScheduleTask>
          rowKey="id"
          size="middle"
          loading={listApi.loading}
          dataSource={shown}
          columns={columns}
          scroll={{ x: 1400 }}
          pagination={shown.length > 20 ? { pageSize: 20, showSizeChanger: false } : false}
          locale={{ emptyText: '还没有定时任务。点右上角「新增任务」创建第一个。' }}
        />
      </SectionCard>

      {/* ---------------- 新建 / 编辑 ---------------- */}
      <Modal
        open={open}
        title={editing ? '编辑定时任务' : '新增定时任务'}
        width={760}
        onCancel={() => setOpen(false)}
        onOk={() => void save()}
        okText="保存"
        confirmLoading={saving}
        destroyOnClose
        styles={{ body: { maxHeight: 'calc(100vh - 260px)', overflowY: 'auto' } }}
      >
        <Form layout="vertical" size="middle">
          <Form.Item label="任务名" required style={{ marginBottom: 14 }}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="例如：夜间打包 / 每天发最新到预生产"
              maxLength={64}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              这个名字会显示在构建/部署记录的「操作人」上，方便事后区分「是不是定时发的」
            </Text>
          </Form.Item>

          <Form.Item label="做什么" style={{ marginBottom: 14 }}>
            <Segmented
              value={kind}
              onChange={(v) => setKind(v as ScheduleTaskType)}
              options={[
                { label: '定时构建', value: 'build' },
                { label: '定时部署', value: 'deploy' },
              ]}
            />
          </Form.Item>

          {kind === 'deploy' ? (
            <>
              <Form.Item label="环境" required style={{ marginBottom: 14 }}>
                <Select
                  value={envKey || undefined}
                  onChange={setEnvKey}
                  placeholder="发到哪个环境"
                  options={envs.map((e) => ({ value: e.key, label: `${e.name}（${e.key}）` }))}
                  style={{ width: '100%' }}
                />
              </Form.Item>
              <Form.Item label="发哪个版本" required style={{ marginBottom: 14 }}>
                <Segmented
                  value={imageSource}
                  onChange={(v) => setImageSource(v as 'fixed' | 'latest')}
                  options={[
                    { label: '每次取最新构建', value: 'latest' },
                    { label: '每个服务指定版本', value: 'fixed' },
                  ]}
                />
                <Text type="secondary" style={{ display: 'block', marginTop: 6, fontSize: 12 }}>
                  {imageSource === 'latest'
                    ? '每次执行时取每个服务此刻最新的成功构建。选这个就不用在每次构建后回来改任务。'
                    : '给每个服务各选一个固定版本，每次执行都发同一版。'}
                </Text>
              </Form.Item>
            </>
          ) : null}

          {/* ---- 目标（批量）---- */}
          <Form.Item
            label={`目标（${targets.length}）`}
            required
            style={{ marginBottom: 14 }}
            tooltip="一条任务可以同时构建 / 部署多个项目，到点会逐条独立建单：一条失败不影响其余"
          >
            <Select
              mode="multiple"
              showSearch
              optionFilterProp="label"
              value={selectedKeys}
              onChange={(v) => setProjectKeys(v as string[])}
              placeholder="选择项目，可多选（批量）"
              options={projectOptions}
              style={{ width: '100%' }}
              maxTagCount="responsive"
            />
            {targets.length ? (
              <div
                style={{
                  marginTop: 8,
                  border: '1px solid var(--yn-border)',
                  borderRadius: 8,
                  overflow: 'hidden',
                }}
              >
                <div
                  style={{
                    padding: '6px 12px',
                    background: 'var(--yn-bg)',
                    fontSize: 12,
                    color: 'var(--yn-text-3, #8c8c8c)',
                  }}
                >
                  {kind === 'build' ? '每个项目用哪个分支构建' : '每个服务发哪个版本'}
                  {kind === 'deploy' && imageSource === 'latest' ? '（当前为「每次取最新」）' : ''}
                </div>
                {targets.map((r) => (
                  <TargetRowEditor
                    key={r.projectKey}
                    row={r}
                    kind={kind}
                    needVersion={kind === 'deploy' && imageSource === 'fixed'}
                    project={projByKey.get(r.projectKey)}
                    onChange={(patch) => patchRow(r.projectKey, patch)}
                    onRemove={() => setProjectKeys(selectedKeys.filter((k) => k !== r.projectKey))}
                  />
                ))}
              </div>
            ) : (
              <Text type="secondary" style={{ fontSize: 12 }}>
                先在上面选项目；选完之后每个项目下面会出现一行，用来选它自己的分支 / 版本
              </Text>
            )}
          </Form.Item>

          <Form.Item label="什么时候跑" required style={{ marginBottom: 8 }}>
            <Space size={8} wrap style={{ marginBottom: 8 }}>
              {CRON_PRESETS.map((p) => (
                <Button key={p.cron} size="small" onClick={() => setCron(p.cron)}>
                  {p.label}
                </Button>
              ))}
            </Space>
            <Input
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              className="yn-mono"
              placeholder="分 时 日 月 周，例如 0 2 * * *"
            />
          </Form.Item>
          <div style={{ marginBottom: 14, minHeight: 42 }}>
            {preview?.valid ? (
              <Space size={6} wrap>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  接下来：
                </Text>
                {preview.next.slice(0, 4).map((t) => (
                  // 去掉秒：`2026-10-30 02:00:00` 里那个 :00 只是噪音，
                  // 而 4 个时间并排时它占的宽度恰好把一行挤满。
                  <Tag key={t} style={{ fontSize: 12 }}>
                    {fmt(t).slice(0, 16)}
                  </Tag>
                ))}
              </Space>
            ) : preview && !preview.valid ? (
              <Text type="danger" style={{ fontSize: 12 }}>
                {preview.error}
              </Text>
            ) : (
              <Text type="secondary" style={{ fontSize: 12 }}>
                <ClockCircleOutlined /> 正在校验表达式…
              </Text>
            )}
          </div>

          <Form.Item label="备注" style={{ marginBottom: 14 }}>
            <Input
              value={remark}
              onChange={(e) => setRemark(e.target.value)}
              placeholder="为什么配这个定时（例如：每天凌晨出一次包给测试用）"
              maxLength={255}
            />
          </Form.Item>

          <Form.Item label="启用" style={{ marginBottom: 0 }}>
            <Switch checked={enabled} onChange={setEnabled} />
            <Text type="secondary" style={{ fontSize: 12, marginLeft: 10 }}>
              停用后不会自动执行，但任务保留（可以随时再打开）
            </Text>
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
};

export default ScheduleList;
