import { defineConfig } from 'vite';
import preact from '@preact/preset-vite';
import tailwindcss from '@tailwindcss/vite';

export default defineConfig({
  plugins: [preact(), tailwindcss()],
  root: '.',
  base: '/dashboard/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      '/webrtc': 'http://localhost:8080',
      '/health': 'http://localhost:8080',
      '/api': 'http://localhost:8080',
    },
  },
});
