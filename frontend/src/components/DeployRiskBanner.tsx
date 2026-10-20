import React, { useEffect, useState } from 'react';
import { Alert, Skeleton } from 'antd';
import { SafetyCertificateOutlined } from '@ant-design/icons';
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
 * ⚠️ **三档都要显示，包括 low**。
 *
 * 最初只显示 medium/high，理由是"低风险是默认态，挂一条是噪音"——**这个判断错了**：
 * 用户打开弹窗看到**什么都没有**，第一反应不是"哦这次很安全"，而是"功能没做"
 *（2026-10-20 实测：`aiobasic@test-152` 18 分 low，弹窗里一片空白，用户直接来问"做了吗"）。
 *
 * 低风险也是一条**有用的信息**："这次没什么特别的"，而且它顺带证明这个功能在工作。
 * 所以 low 也渲染，只是收敛成一行小字，不抢镜。
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
  // 还没算出来 / 评估失败：不渲染（评估是辅助信息，不能拦住部署）
  if (!level) return null;

  if (level === 'low') {
    // ⚠️ 要**一眼看到有这个东西**，但又不能做得像警告 ——
    // 它是绿色、带左侧色条、分数大一号；原因单独一行弱化。
    // （2026-10-20 用户先反馈"看不到"、再反馈"太不明显"，两次都是同一个问题：
    //   信息的存在感要和它的重要性匹配，而"低"也不等于"可以忽略"。）
    return (
      <div className="yn-risk-low">
        <SafetyCertificateOutlined className="yn-risk-low__icon" />
        <div style={{ minWidth: 0 }}>
          <div className="yn-risk-low__head">
            <span className="yn-risk-low__title">风险低</span>
            <span className="yn-risk-low__score">{score}</span>
            <span className="yn-risk-low__unit">分</span>
          </div>
          <div className="yn-risk-low__reasons">
            {reasons.length ? reasons.join('；') : '没有命中任何风险因子'}
          </div>
        </div>
      </div>
    );
  }

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
