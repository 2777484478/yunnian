import React from 'react';

/**
 * 项目 logo 的图形部分（白色云），置于橙色渐变圆角方块内使用。
 *
 * 与浏览器标签页图标（`frontend/public/favicon.svg`，同路径已内联进 `index.html` 的 data URI）
 * 共用同一份矢量：改这里的同时记得同步 favicon.svg，两处保持一致。
 */
export const LogoCloud: React.FC<{ size?: number; className?: string }> = ({ size = 18, className }) => (
  <svg
    className={className}
    width={size}
    height={size}
    viewBox="0 0 24 24"
    fill="currentColor"
    aria-hidden="true"
    focusable="false"
  >
    <path d="M18 10h-1.26A8 8 0 1 0 9 20h9a5 5 0 0 0 0-10z" />
  </svg>
);

export default LogoCloud;
