import react from '@vitejs/plugin-react'
import { defineConfig, loadEnv } from 'vite'

// 本地代理将 /api 转发至 Gateway，浏览器无需跨域，也不接触后端密钥。
export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), '')
  return { plugins: [react()], server: { proxy: { '/api': { target: env.GATEWAY_PROXY_TARGET || 'http://127.0.0.1:8080', changeOrigin: true } } } }
})
