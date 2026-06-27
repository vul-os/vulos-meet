import { writeFileSync } from 'node:fs'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// emptyOutDir wipes dist/ on every build, including the committed dist/.gitkeep
// placeholder that lets `go build` (//go:embed all:dist) compile before any
// frontend build exists. Recreate it after the bundle is written so the embed
// directive always has a file to match.
const keepGitkeep = {
  name: 'keep-dist-gitkeep',
  closeBundle() {
    writeFileSync('dist/.gitkeep', '')
  },
}

// Vulos Meet client: a single SPA build (dist/) embedded into the Go binary and
// served from the public signal-gate listener. base "/" — same origin as /rtc.
export default defineConfig({
  base: '/',
  plugins: [react(), keepGitkeep],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 2000,
  },
  test: {
    environment: 'jsdom',
    globals: true,
    include: ['src/**/*.test.{js,jsx}'],
  },
  server: {
    port: 5176,
  },
})
