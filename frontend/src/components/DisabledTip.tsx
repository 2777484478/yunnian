import { Tooltip } from 'antd';
import type { ReactNode } from 'react';

/**
 * 给「灰着的按钮」补一句说明。
 *
 * 为什么必须有这一层：antd 的 Tooltip **不会**替 disabled 子元素兜底。
 * disabled 的 `<button>` 不派发鼠标事件（pointer-events 也不被子元素继承式恢复），
 * 所以 `<Tooltip><Button disabled/></Tooltip>` 永远不会弹出——
 * 结果就是"唯一能解释为什么点不动的地方"恰好对最需要它的人失效。
 * 用一层 inline-block 的 span 承接 hover 事件即可。
 *
 * 用法：`<DisabledTip title={ok ? undefined : '需要 xxx 权限'}><Button disabled={!ok}/></DisabledTip>`
 */
export function DisabledTip({ title, children }: { title?: ReactNode; children: ReactNode }) {
  if (!title) return <>{children}</>;
  return (
    <Tooltip title={title}>
      <span style={{ display: 'inline-block' }}>{children}</span>
    </Tooltip>
  );
}

export default DisabledTip;
