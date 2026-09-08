import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // Dev: forward API requests to the Go backend.
      '/api': 'http://localhost:8080',
    },
  },
})
