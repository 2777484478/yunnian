import { fileURLToPath, URL } from 'node:url';
import { readFileSync } from 'node:fs';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { viteSingleFile } from 'vite-plugin-singlefile';

// 版本号与构建时刻注入到前端：侧边栏要显示"这一版到底是什么"。
// 直接读 package.json 而不是用环境变量——`npm run build` 与 `vite build`
// 两种调用方式下 npm_ 前缀的变量是否存在并不一致，读文件最稳。
const pkg = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf-8')) as {
  version: string;
};

// 说明：
// - viteSingleFile 让 `npm run build` 产出一个自包含的 dist/index.html（脚本/样式全部内联），
//   方便把原型页直接丢给同事/评审查看，无需任何静态服务器。
// - 正式联调时用 `npm run dev`，走下面的 /api 代理指向 Go 后端（默认 :8080）。
//   端口必须与后端 CORS 白名单一致（见 configs/config.yaml 的 corsOrigins），
//   否则浏览器会拦在预检上，表现为"接口明明通、页面却报跨域"。
export default defineConfig({
  plugins: [react(), viteSingleFile()],
  define: {
    __APP_VERSION__: JSON.stringify(pkg.version),
    __BUILD_TIME__: JSON.stringify(new Date().toISOString().slice(0, 10)),
  },
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  base: './',
  build: {
    outDir: 'dist',
    cssCodeSplit: false,
    assetsInlineLimit: 100000000,
    chunkSizeWarningLimit: 8000,
  },
  server: {
    port: 5173,
    host: '127.0.0.1',
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: true,
        // SSE 日志流必须关掉缓冲，否则日志会攒着一次性吐出来，实时感全无
        ws: false,
      },
    },
  },
});
