import React from 'react';
import { Tooltip } from 'antd';

export interface PickedStripProps {
  /** 已勾选的项（顺序即展示顺序，一般与用户勾选顺序一致） */
  items: { key: string; name: string }[];
  /** 点一下移出。不传则纯只读展示 */
  onRemove?: (key: string) => void;
}

/**
 * 已勾选项目的**灰色清单**（批量打包 / 批量发布共用）。
 *
 * 为什么需要它：列表带搜索时，「已选 N 个」只是一个数字 ——
 * 搜 `aioai` 的时候看不到上一轮搜 `aioitms` 勾的那一个，
 * 用户没法确认自己到底攒了哪些项目。
 * （上一轮"跨搜索勾选丢失"的 bug 之所以难察觉，也正是因为只看得见数字：
 *   界面一直显示"已选 1 个"，看着像是勾选没生效。）
 *
 * 两个刻意的选择：
 *   · **灰色、小字、等宽**：它是**状态回显**，不是操作入口。做成彩色 Tag 会和上方那些
 *     统计标签（"有更新 3 个"、"未纳管 2 个"）抢注意力，而后者才是需要用户先看的。
 *   · **可点移除**：发现选错了不必回去搜索定位、取消勾选再清空搜索 —— 直接点掉。
 *     所以有 onRemove 时给 hover 提示，不能让它"看起来不能点、其实能点"。
 */
export const PickedStrip: React.FC<PickedStripProps> = ({ items, onRemove }) => {
  if (!items.length) return null;
  return (
    <div className="yn-picked-strip">
      {items.map((it) => (
        <Tooltip key={it.key} title={onRemove ? `点一下移出：${it.name}` : it.name}>
          <span
            className={`yn-picked-strip__item ${onRemove ? 'is-removable' : ''}`}
            onClick={onRemove ? () => onRemove(it.key) : undefined}
          >
            {it.name}
          </span>
        </Tooltip>
      ))}
    </div>
  );
};

export default PickedStrip;
