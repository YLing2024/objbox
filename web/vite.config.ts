import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 产物输出到 web/dist，由 //go:embed all:dist 打进二进制。
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
})
