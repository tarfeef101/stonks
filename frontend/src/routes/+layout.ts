// SPA (single-page app): no server-side rendering. The SvelteKit bundle is
// built as static files and served by nginx:alpine, which falls back to
// index.html for client-side routes (see vite.config.ts adapter-static).
export const ssr = false;
