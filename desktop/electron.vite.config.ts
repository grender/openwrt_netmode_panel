import { resolve } from 'node:path'
import { defineConfig, externalizeDepsPlugin } from 'electron-vite'
import preact from '@preact/preset-vite'

export default defineConfig({
  main: { plugins: [externalizeDepsPlugin()] },
  preload: { plugins: [externalizeDepsPlugin()] },
  renderer: {
    plugins: [preact()],
    // style.css imports the panel's stylesheet from web/panel (one design system).
    server: { fs: { allow: [resolve(__dirname, '..')] } },
  },
})
