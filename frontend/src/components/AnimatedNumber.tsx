import React, { useEffect, useRef, useState } from 'react';

// ---------------------------------------------------------------------------
// 数字滚动：入场时从 0 缓动到目标值（easeOutCubic）
// 细节：等宽数字（tabular-nums）避免滚动过程中宽度跳动导致相邻元素抖动
// ---------------------------------------------------------------------------

export interface AnimatedNumberProps {
  value: number;
  /** 小数位，不传则按传入值自动推断 */
  decimals?: number;
  /** 动画时长（毫秒） */
  duration?: number;
  /** 是否千分位分隔，默认开 */
  thousands?: boolean;
  prefix?: string;
  suffix?: string;
}

export const AnimatedNumber: React.FC<AnimatedNumberProps> = ({
  value,
  decimals,
  duration = 900,
  thousands = true,
  prefix = '',
  suffix = '',
}) => {
  const [display, setDisplay] = useState(0);
  const rafRef = useRef<number | null>(null);

  const dp = decimals ?? (Number.isInteger(value) ? 0 : (String(value).split('.')[1]?.length ?? 0));

  useEffect(() => {
    const to = value;
    const startedAt = performance.now();

    const tick = (now: number) => {
      const p = Math.min(1, (now - startedAt) / duration);
      const eased = 1 - Math.pow(1 - p, 3);
      setDisplay(to * eased);
      if (p < 1) rafRef.current = requestAnimationFrame(tick);
    };

    rafRef.current = requestAnimationFrame(tick);
    return () => {
      if (rafRef.current !== null) cancelAnimationFrame(rafRef.current);
    };
  }, [value, duration]);

  const raw = display.toFixed(dp);
  const [int, frac] = raw.split('.');
  const pretty = thousands ? `${int.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}${frac ? `.${frac}` : ''}` : raw;

  return (
    <span className="yn-num">
      {prefix}
      {pretty}
      {suffix}
    </span>
  );
};

export default AnimatedNumber;
