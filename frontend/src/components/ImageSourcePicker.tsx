import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Alert, Segmented, Select, Space, Tag, Typography } from 'antd';
import { CloudUploadOutlined, HddOutlined } from '@ant-design/icons';
import { imageApi, registryApi } from '@/api';
import type { ImageRecord } from '@/types';
import { useApi } from '@/hooks/useApi';

const { Text } = Typography;

/**
 * 「镜像来源」选择器 —— 部署时**二选一**。
 *
 *  平台构建  该项目的 image_record：平台自己构建过的版本，带构建号 / commit / 推送时间
 *  仓库标签  镜像仓库里**实际存在**的标签（含别的平台推的、别人手工推的）
 *
 * ⚠️ 两条来源都要保留、**只能选一个**。这不是界面上的洁癖：后端
 * `checkImageSource` 对"两个都给"和"一个都不给"都会拒 ——
 * 界面上如果允许同时选，用户点了确认只会拿到一个 400，那是"能勾选≠后端受理"。
 * 所以这里切组就清空已选值。
 *
 * 作为**受控组件**直接放进 Form.Item（value / onChange）。
 *
 * ⚠️ 为什么抽成一个组件而不是两处各写一份：环境详情与拓扑页都有发布入口，
 * 各写一份的下场已经见过了（环境级动作就曾因为"定义在视图层、视图有两个"而漂移，
 * 见 EnvironmentActions）。镜像来源的**口径**比按钮更容易漂：一处按推送时间排、
 * 另一处按名字排，用户看到的两份列表就不一样。
 */

/** 选中的镜像。带上 image 是为了让确认页 / 详情页直接显示完整镜像，不必再查一次。 */
export type ImageChoice =
  | { source: 'platform'; imageRecordId: string; tag: string; image: string }
  | { source: 'registry'; registryTag: string; tag: string; image: string };

interface Props {
  /** 当前项目标识；为空时两组都禁用（还不知道要读哪个项目的仓库）。 */
  projectKey?: string;
  value?: ImageChoice;
  onChange?: (v: ImageChoice | undefined) => void;
  disabled?: boolean;
  /** 仓库标签一次列多少个（缺省 5）。 */
  limit?: number;
}

const SOURCE_KEY = 'yn.deploy.imageSource';

/** 记住上次用的来源：同一个人反复发版时不用每次重新点一遍。 */
function readLastSource(): 'platform' | 'registry' {
  try {
    return localStorage.getItem(SOURCE_KEY) === 'registry' ? 'registry' : 'platform';
  } catch {
    return 'platform';
  }
}

function writeLastSource(s: 'platform' | 'registry') {
  try {
    localStorage.setItem(SOURCE_KEY, s);
  } catch {
    // 隐私模式 / 存储被禁：记不住就不记，不影响这一页能用
  }
}

/** 把时间按 timeKind 说清楚；拿不到时间就显 "—"。 */
function timeText(t: { time: string; timeKind: string }): string {
  if (!t.time) return '时间未知';
  const label = t.timeKind === 'build' ? '构建于' : t.timeKind === 'push' ? '推送于' : '';
  const when = t.time.slice(0, 19).replace('T', ' ');
  return label ? `${label} ${when}` : when;
}

const ImageSourcePicker: React.FC<Props> = ({ projectKey, value, onChange, disabled, limit = 5 }) => {
  const [source, setSource] = useState<'platform' | 'registry'>(() =>
    value?.source ?? readLastSource(),
  );

  // 平台构建：该项目最近 50 条镜像记录（按推送时间倒序）
  const imagesApi = useApi(() => imageApi.list({ projectKey: projectKey!, pageSize: 50 }), [projectKey], {
    enabled: !!projectKey && source === 'platform',
  });
  const images = useMemo(() => imagesApi.data?.list ?? [], [imagesApi.data]);

  // 仓库标签：只有切到这一组才去问（不在打开弹窗时就多打一次仓库接口）
  const tagsApi = useApi(() => registryApi.harborTags(projectKey!, limit), [projectKey, limit], {
    enabled: !!projectKey && source === 'registry',
  });
  const reg = tagsApi.data;

  /**
   * 换项目必须**清掉已选值**。
   *
   * 不清的后果不是"选了个空"，而是把 A 项目的镜像发到 B 项目上 ——
   * 前端看着只是下拉里的值没了，后端却会真拿这个 id/tag 去部署。
   * （历史上"切换项目后 Select 的 value 留在上一个项目"真发生过一次，
   * 所以后端也留了项目校验兜底；这里两边都必须拦。）
   */
  const lastProject = useRef(projectKey);
  useEffect(() => {
    if (lastProject.current === projectKey) return;
    lastProject.current = projectKey;
    onChange?.(undefined);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectKey]);

  // 外部把值清空（如父级重置表单）时，让内部分组也跟着回到默认，避免"显示仓库组但没选中任何东西"
  useEffect(() => {
    if (!value) setSource(readLastSource());
  }, [value]);

  const pickSource = (s: 'platform' | 'registry') => {
    if (s === source) return;
    setSource(s);
    writeLastSource(s);
    // 二选一：切组即清空，绝不把上一组的选中值带过去
    onChange?.(undefined);
  };

  const platformOptions = images.map((i: ImageRecord, idx) => ({
    label: `${i.tag}${idx === 0 ? '（最新）' : ''}  ·  ${i.pushedAt?.slice(0, 19).replace('T', ' ') ?? '—'}`,
    value: i.id,
    choice: {
      source: 'platform' as const,
      imageRecordId: String(i.id),
      tag: i.tag,
      // 记录里就存着完整镜像串，直接用；没有就按 registry/repo:tag 拼
      image: i.image || `${i.projectKey}:${i.tag}`,
    },
  }));

  const registryOptions = (reg?.tags ?? []).map((t) => ({
    label: `${t.tag}  ·  ${timeText(t)}`,
    value: t.tag,
    choice: {
      source: 'registry' as const,
      registryTag: t.tag,
      tag: t.tag,
      image: reg?.imageBase ? `${reg.imageBase}:${t.tag}` : t.tag,
    },
  }));

  const platformSelect = (
    <Select
      showSearch
      optionFilterProp="label"
      style={{ width: '100%' }}
      placeholder={projectKey ? '选择平台构建的版本' : '请先选择项目'}
      disabled={disabled || !projectKey}
      loading={imagesApi.loading}
      value={value?.source === 'platform' ? value.imageRecordId : undefined}
      options={platformOptions.map(({ label, value: v }) => ({ label, value: v }))}
      onChange={(v) => {
        const hit = platformOptions.find((o) => o.value === v);
        onChange?.(hit?.choice);
      }}
      notFoundContent={
        imagesApi.loading ? '加载中…' : projectKey ? '该项目暂无平台构建产物' : '请先选择项目'
      }
    />
  );

  const registrySelect = (
    <Select
      showSearch
      optionFilterProp="label"
      style={{ width: '100%' }}
      placeholder={projectKey ? `选择仓库里的标签（最近 ${limit} 个）` : '请先选择项目'}
      disabled={disabled || !projectKey || !!reg?.unavailable}
      loading={tagsApi.loading}
      value={value?.source === 'registry' ? value.registryTag : undefined}
      options={registryOptions.map(({ label, value: v }) => ({ label, value: v }))}
      onChange={(v) => {
        const hit = registryOptions.find((o) => o.value === v);
        onChange?.(hit?.choice);
      }}
      notFoundContent={
        tagsApi.loading ? '加载中…' : reg?.unavailable ? '读不到仓库里的标签（见下方说明）' : '没有标签'
      }
    />
  );

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={6}>
      <Segmented
        value={source}
        onChange={(v) => pickSource(v as 'platform' | 'registry')}
        disabled={disabled}
        options={[
          { value: 'platform', label: '平台构建', icon: <HddOutlined /> },
          { value: 'registry', label: '仓库标签', icon: <CloudUploadOutlined /> },
        ]}
      />

      {source === 'platform' ? platformSelect : registrySelect}

      {source === 'platform' ? (
        <Text type="secondary" style={{ fontSize: 12 }}>
          来自平台自己构建过的镜像记录，带构建号与提交号可溯源。
        </Text>
      ) : null}

      {/* 仓库这一组必须把**来源与时间口径**讲清楚：读不到要说原因，
          时间不是推送时间也要说 —— 否则用户会把"构建于"当成"推送于"。 */}
      {source === 'registry' ? (
        <>
          {reg?.unavailable ? (
            <Alert type="warning" showIcon style={{ fontSize: 12 }} message={reg.unavailable} />
          ) : null}
          {reg && !reg.unavailable ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              <span className="yn-mono">{reg.repo}</span>
              {reg.note ? ` · ${reg.note}` : ''}
              {reg.source === 'search' ? <Tag style={{ marginLeft: 6 }}>按项目标识检索</Tag> : null}
            </Text>
          ) : null}
        </>
      ) : null}
    </Space>
  );
};

export default ImageSourcePicker;
