/* eslint-env node */
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { fileURLToPath } from 'node:url'

const frontend = fileURLToPath(new URL('../', import.meta.url))
if (!process.env.DEVIN_JOURNEY_UI_DIST) throw new Error('Set an isolated DEVIN_JOURNEY_UI_DIST directory')
export default defineConfig({
  root: frontend,
  plugins: [vue()],
  resolve: { alias: { '@': fileURLToPath(new URL('../src', import.meta.url)) } },
  build: {
    outDir: process.env.DEVIN_JOURNEY_UI_DIST,
    emptyOutDir: false,
    rollupOptions: { input: fileURLToPath(new URL('./devin-handoff.html', import.meta.url)) }
  }
})
