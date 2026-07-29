import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import path from 'node:path'

// vite.config.ts — 管理后台构建配置 (FE-ADMIN-01)
// dev server proxy: /v1 → http://localhost:8080 (后端 Go 单体, 避免 CORS)
// 路径别名 @/ → src/ (与 tsconfig.paths 对齐)
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
  server: {
    host: '0.0.0.0',
    port: 5173,
    proxy: {
      '/v1': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    target: 'es2020',
  },
})
