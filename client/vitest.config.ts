import { defineConfig } from 'vitest/config';

// Standalone vitest config: deliberately does NOT load the vite.config.ts
// preact/PWA plugins — tests here are pure protocol/unit tests (node env).
export default defineConfig({
  test: {
    environment: 'node',
    include: ['src/**/*.test.ts'],
  },
});
