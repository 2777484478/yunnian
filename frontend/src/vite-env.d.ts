/// <reference types="vite/client" />

// 说明：本文件让 `import.meta.env.VITE_*` 有类型。
// 之前项目里没有它，是因为原型阶段没读过任何环境变量；
// 接口层要用 VITE_API_BASE 覆盖后端地址，才需要补上。

interface ImportMetaEnv {
  /** 覆盖默认接口前缀（默认 /api/v1，开发时走 vite 代理） */
  readonly VITE_API_BASE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}

// 构建期注入的常量（见 vite.config.ts 的 define）。
// 侧边栏用它们显示"这一版是什么版本、什么时候构建的"。
declare const __APP_VERSION__: string;
declare const __BUILD_TIME__: string;
