import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Badge,
  Button,
  Input,
  Segmented,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  FolderOutlined,
  ReloadOutlined,
  SearchOutlined,
  SettingOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { PageHeader, SectionCard } from '@/components/ui';
import { ApiError, jenkinsApi } from '@/api';
import type { JenkinsJob, JenkinsJobList } from '@/api';
import { fmt } from '@/utils/format';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// Jenkins Job 列表页
//
// 这个页面是「复用现有 Jenkins」这条路的入口：不去把 Jenkins 的 job 重新建模成
// 平台项目，而是把 Jenkins 上已有的 job 拉过来看、点着跑。
//
// 它是**独立菜单项**而不是项目管理里的一个抽屉。原因很实际：真实的 Jenkins
// （实测那台）有 **531 个 job**，那是一个需要搜索、筛选、翻页的正式列表，
// 塞进抽屉里看不完也筛不动。项目管理只保留「这个项目用哪个 job」的绑定关系。
//
// 三处刻意的设计：
//
//   1. **列表以完整路径为准，不是裸 job 名**。用了 Folders 插件之后
//      `backend/pay/ci-payment` 和顶层的 `ci-payment` 是两个不同的 job，
//      只显示裸名会让人点错一个。因此主列显示完整路径、并单独标出文件夹。
//      （目标 Jenkins 目前没启用文件夹，但代码必须两种都能处理——否则一旦
//      对方启用 Folders，平台会静默显示错名字。）
//
//   2. **这一页只读 Jenkins，没有任何写动作。**
//      构建入口统一收在「项目管理 › 编译」里：平台要靠项目登记这次构建的镜像、
//      日志与历史，后端也只受理**已绑定平台项目**的 Job（未绑定回 40901）。
//      在这里放一个"点一下就构建"的按钮，等于对 531 个未绑定的 job 做了个
//      点得动、发不出的承诺，还容易让人以为绕过项目也能构建。
//      所以本页只做浏览、筛选、以及跳到 Jenkins 页面自己构建。
//
//   3. **绑定关系在本页只读展示**。Job 与项目的绑定在项目编辑里维护，
//      Jenkins 本身并不知道平台的存在，所以这里只是把平台侧的标注显示出来。
// ---------------------------------------------------------------------------

/**
 * Jenkins 的 job 状态色 → 中文语义。
 *
 * ⚠️ 注意这套颜色是**反直觉**的：blue = 成功、red = 失败、yellow = 不稳定，
 * 而不是"蓝色中性/红色告警"。这是 Jenkins/Hudson 的历史命名（蓝色小球代表通过），
 * 不能按常规直觉去映射，否则会把"成功"显示成"未知"。
 */
const jobStateOf = (color: string): { text: string; badge: 'success' | 'error' | 'warning' | 'processing' | 'default' } => {
  if (color.endsWith('_anime')) return { text: '构建中', badge: 'processing' };
  const base = color.replace(/_anime$/, '');
  switch (base) {
    case 'blue':
      return { text: '成功', badge: 'success' };
    case 'red':
      return { text: '失败', badge: 'error' };
    case 'yellow':
      return { text: '不稳定', badge: 'warning' };
    case 'aborted':
      return { text: '已中止', badge: 'default' };
    case 'grey':
    case 'disabled':
      return { text: '未构建', badge: 'default' };
    default:
      return { text: base || '未知', badge: 'default' };
  }
};

/** Jenkins 构建结果 → 中文 */
const RESULT_TEXT: Record<string, string> = {
  SUCCESS: '成功',
  FAILURE: '失败',
  UNSTABLE: '不稳定',
  ABORTED: '已中止',
  NOT_BUILT: '未执行',
};

const JenkinsJobList: React.FC = () => {
  const navigate = useNavigate();

  const [data, setData] = useState<JenkinsJobList | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [keyword, setKeyword] = useState('');
  /** 按 job 状态筛（成功/失败/构建中…）。空串 = 不筛。 */
  const [stateFilter, setStateFilter] = useState<string>('');
  const [scope, setScope] = useState<'all' | 'bound' | 'unbound'>('all');

  const load = useCallback(async () => {
    setLoading(true);
    try {
      // refresh=1：手动点刷新时让后端跳过缓存。
      // 不加这个的话"点了刷新没变化"会被当成刷新没生效（Jenkins 侧确实在变）。
      const res = await jenkinsApi.jobs({ refresh: true });
      setData(res);
      setError(null);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : '拉取 Jenkins Job 列表失败');
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const jobs = data?.jobs ?? [];

  /** 列表里实际出现过的状态，只把它们放进筛选器——不放永远筛不出东西的选项 */
  const stateOptions = useMemo(() => {
    const seen = new Set(jobs.map((j) => jobStateOf(j.color).text));
    return [...seen].map((t) => ({ label: t, value: t }));
  }, [jobs]);

  const rows = useMemo(() => {
    const kw = keyword.trim().toLowerCase();
    return jobs.filter((j) => {
      if (scope === 'bound' && !j.boundProjectKey) return false;
      if (scope === 'unbound' && j.boundProjectKey) return false;
      if (stateFilter && jobStateOf(j.color).text !== stateFilter) return false;
      if (!kw) return true;
      return j.fullName.toLowerCase().includes(kw);
    });
  }, [jobs, keyword, scope, stateFilter]);

  const folderCount = useMemo(() => new Set(jobs.filter((j) => j.folder).map((j) => j.folder)).size, [jobs]);
  const buildingCount = useMemo(() => jobs.filter((j) => j.lastBuild?.building).length, [jobs]);
  const boundCount = useMemo(() => jobs.filter((j) => j.boundProjectKey).length, [jobs]);

  const columns: ColumnsType<JenkinsJob> = [
    {
      title: 'Job',
      dataIndex: 'fullName',
      render: (_: unknown, r) => (
        <div>
          <Space size={6}>
            {r.folder ? (
              <FolderOutlined style={{ color: '#ff9c6e' }} />
            ) : (
              <ThunderboltOutlined style={{ color: '#1677ff' }} />
            )}
            <Text strong className="yn-mono" style={{ fontSize: 13 }}>
              {/* Job 名就是去 Jenkins 的链接：这一页没有写动作，
                  唯一的"动手"就是跳到 Jenkins 页面上自己构建 */}
              <a href={r.url} target="_blank" rel="noreferrer" title="在 Jenkins 中打开">
                {r.name}
              </a>
            </Text>
            {r.parameterized ? (
              <Tooltip
                title={
                  r.paramNames?.length
                    ? `构建参数：${r.paramNames.join('、')}。要构建请到「项目管理」的编译弹窗里选择版本 / 分支`
                    : '该 Job 有构建参数。要构建请到「项目管理」的编译弹窗里选择版本 / 分支'
                }
              >
                <Tag style={{ marginInlineEnd: 0 }}>参数化</Tag>
              </Tooltip>
            ) : null}
          </Space>
          {/* 文件夹路径单独一行：跟在名字后面会把名字挤到看不全 */}
          {r.folder ? (
            <div>
              <Text type="secondary" className="yn-mono" style={{ fontSize: 11.5 }}>
                {r.folder}/
              </Text>
            </div>
          ) : null}
        </div>
      ),
    },
    {
      title: '状态',
      dataIndex: 'color',
      width: 92,
      render: (color: string) => {
        const s = jobStateOf(color);
        return <Badge status={s.badge} text={s.text} />;
      },
    },
    {
      title: '最近构建',
      dataIndex: 'lastBuild',
      width: 200,
      render: (_: unknown, r) => {
        const b = r.lastBuild;
        if (!b) {
          return (
            <Text type="secondary" style={{ fontSize: 12 }}>
              未构建过
            </Text>
          );
        }
        if (b.building) {
          return (
            <Space size={6}>
              <Text className="yn-mono" style={{ fontSize: 12 }}>
                #{b.number}
              </Text>
              <Tag color="processing" style={{ marginInlineEnd: 0 }}>
                进行中
              </Tag>
            </Space>
          );
        }
        const secs = Math.round(b.duration / 1000);
        return (
          <Space size={6} wrap>
            <Text className="yn-mono" style={{ fontSize: 12 }}>
              #{b.number}
            </Text>
            <Tag
              color={b.result === 'SUCCESS' ? 'success' : b.result === 'UNSTABLE' ? 'warning' : 'error'}
              style={{ marginInlineEnd: 0 }}
            >
              {RESULT_TEXT[b.result ?? ''] ?? b.result}
            </Tag>
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              耗时 {secs >= 60 ? `${Math.floor(secs / 60)}m${secs % 60}s` : `${secs}s`}
            </Text>
          </Space>
        );
      },
    },
    {
      title: '绑定项目',
      dataIndex: 'boundProjectKey',
      width: 150,
      render: (key: string | null) =>
        key ? (
          <Tooltip title="该项目在平台的构建方式已选为此 Job">
            <Tag color="blue" className="yn-mono" style={{ marginInlineEnd: 0 }}>
              {key}
            </Tag>
          </Tooltip>
        ) : (
          <Text type="secondary" style={{ fontSize: 12 }}>
            未绑定
          </Text>
        ),
    },
  ];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CI 镜像构建' }, { title: 'Jenkins Job' }]}
        title="Jenkins Job"
        description="浏览 Jenkins 上已有的 Job、看它们最近构建的状态与绑定关系。本页只读取 Jenkins，不触发构建、也不会修改 Job 定义、插件或全局配置；要构建请到「项目管理」里对具体项目发起（平台上每次构建都要落到一个项目上）。"
        tags={
          data ? (
            <Tag color={data.source === 'demo' ? 'orange' : 'blue'} style={{ marginInlineEnd: 0 }}>
              {data.source === 'demo' ? '演示数据' : '实时数据'}
            </Tag>
          ) : null
        }
        extra={
          <Space>
            <Tooltip title="接入地址、用户名与凭据在「系统管理 › 第三方集成」里配置">
              <Button icon={<SettingOutlined />} onClick={() => navigate('/system/integrations')}>
                接入设置
              </Button>
            </Tooltip>
            <Tooltip title="重新拉取 Job 列表（跳过缓存）">
              <Button icon={<ReloadOutlined />} loading={loading} onClick={() => void load()}>
                刷新
              </Button>
            </Tooltip>
          </Space>
        }
      />

      {data && data.skippedFolders > 0 ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message={`有 ${data.skippedFolders} 个文件夹因权限不足未能读取`}
          description="Jenkins 账号需要 Overall/Read 权限才能遍历文件夹内的 Job。当前列表可能不完整。"
        />
      ) : null}

      {data && data.truncated ? (
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message="Job 数量超过平台单次拉取上限，列表已截断"
          description="请用下方搜索框按名称筛选，或调高平台侧的单次拉取上限。"
        />
      ) : null}

      {error ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 12 }}
          message="拉取 Job 列表失败"
          description={error}
          action={
            <Button size="small" onClick={() => void load()}>
              重试
            </Button>
          }
        />
      ) : null}

      <SectionCard bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索 Job 名称 / 文件夹路径"
              style={{ width: 260 }}
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
            />
            <Select
              allowClear
              placeholder="全部状态"
              style={{ width: 130 }}
              value={stateFilter || undefined}
              onChange={(v) => setStateFilter(v ?? '')}
              options={stateOptions}
            />
            <Segmented
              value={scope}
              onChange={(v) => setScope(v as typeof scope)}
              options={[
                { label: `全部 ${jobs.length}`, value: 'all' },
                { label: `已绑定 ${boundCount}`, value: 'bound' },
                { label: `未绑定 ${jobs.length - boundCount}`, value: 'unbound' },
              ]}
            />
            {/* 数量与筛选结果一起显示：531 个 job 的列表里，"筛出了几个"比"一共有几个"更该被看到 */}
            <Text type="secondary" style={{ fontSize: 12 }}>
              {rows.length === jobs.length ? `共 ${jobs.length} 个` : `筛选出 ${rows.length} / ${jobs.length} 个`}
              {buildingCount > 0 ? ` · ${buildingCount} 个构建中` : ''}
              {folderCount > 0 ? ` · 分布在 ${folderCount} 个文件夹` : ''}
              {data?.fetchedAt ? ` · 更新于 ${fmt(data.fetchedAt)}` : ''}
            </Text>
          </Space>
        </div>

        <Table<JenkinsJob>
          size="small"
          rowKey="fullName"
          loading={loading && !data}
          dataSource={rows}
          columns={columns}
          pagination={{
            defaultPageSize: 20,
            showSizeChanger: true,
            pageSizeOptions: ['20', '50', '100'],
            size: 'small',
            showTotal: (t) => `共 ${t} 个 Job`,
          }}
          locale={{ emptyText: error ? '拉取失败' : '没有匹配的 Job' }}
        />
      </SectionCard>

      <div style={{ marginTop: 12 }}>
        <Text type="secondary" style={{ fontSize: 11.5, lineHeight: 1.9 }}>
          说明：Job 的「绑定项目」关系在<b>项目管理</b>里维护（项目编辑 → 构建方式选 Jenkins Job 后选择）；
          绑定关系只是平台侧的标注，Jenkins 本身并不知道平台的存在。
          本页<b>不会写入 Jenkins</b>：浏览、筛选、翻页都不碰 Jenkins，
          点 <b>Job 名称</b>只是把 Jenkins 页面在新标签页里打开。
          <br />
          <b>要构建请到「项目管理」</b>：在项目的「编译」里选择构建分支——可选值就是这个 Job 的
          版本标签 / 分支（临时向 Jenkins 现取，每次打开都真读一遍仓库，约 1 秒），
          提交后由平台触发 Jenkins 并同步日志与状态。之所以不在本页直接触发：
          平台要靠项目登记这次构建属于谁、镜像登记到哪、日志挂在哪，
          未绑定项目的 Job 后端会直接拒绝（40901），与其让人点了再失败，不如只留下能成的那条路。
        </Text>
      </div>
    </div>
  );
};

export default JenkinsJobList;
