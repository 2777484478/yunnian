import React, { useEffect, useState } from 'react';
import { Alert, Skeleton } from 'antd';
import { deployApi } from '@/api';

export interface DeployRiskBannerProps {
  projectKey?: string;
  envKey?: string;
  /** 打开表单时给 true —— 只有开关时才去算，不是每次渲染都请求 */
  active: boolean;
}

/**
 * 部署风险评估条。
 *
 * 放在**建单之前**的表单里，因为它的全部价值就是"提交前知道有多险"。
 * 放到部署记录列表上没有意义 —— 那时候已经发完了。
 *
 * 只显示 medium / high：`low` 是默认状态，每个表单都挂一条"低风险"只会变成噪音。
 */
const DeployRiskBanner: React.FC<DeployRiskBannerProps> = ({ projectKey, envKey, active }) => {
  const [level, setLevel] = useState<'low' | 'medium' | 'high' | null>(null);
  const [score, setScore] = useState(0);
  const [reasons, setReasons] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (!active || !projectKey || !envKey) {
      setLevel(null);
      return;
    }
    let alive = true;
    setLoading(true);
    deployApi
      .risk(projectKey, envKey)
      .then((r) => {
        if (!alive) return;
        setLevel(r.level);
        setScore(r.score);
        setReasons(r.reasons ?? []);
      })
      .catch(() => {
        // 算不出来就不显示 —— **不能因为"评估失败"而拦住部署**，
        // 那会把一个辅助信息变成硬门槛。
        if (alive) setLevel(null);
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [active, projectKey, envKey]);

  if (loading) return <Skeleton.Input active size="small" style={{ width: '100%', marginBottom: 12 }} block />;
  if (!level || level === 'low') return null;

  return (
    <Alert
      type={level === 'high' ? 'error' : 'warning'}
      showIcon
      style={{ marginBottom: 12 }}
      message={
        <span>
          {level === 'high' ? '高风险部署' : '中等风险'} · 风险分 {score}
        </span>
      }
      description={
        <ul style={{ margin: '4px 0 0', paddingLeft: 18, fontSize: 12.5 }}>
          {reasons.map((r) => (
            <li key={r}>{r}</li>
          ))}
        </ul>
      }
    />
  );
};

export default DeployRiskBanner;
