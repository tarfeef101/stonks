import adapter from '@sveltejs/adapter-static';
import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [
		sveltekit({
			// SPA: every unknown route falls back to index.html so the
			// client-side router can take over on deep links.
			adapter: adapter({
				fallback: 'index.html'
			}),
			compilerOptions: {
				// Force runes mode for the project, except for libraries. Can be removed in svelte 6.
				runes: ({ filename }) =>
					filename.split(/[/\\]/).includes('node_modules') ? undefined : true
			}
		}),
		tailwindcss()
	],
	server: {
		// Dev-only: forward /api to the backend (which serves API routes
		// under /api/... — plain pass-through, no rewrite). Inside the
		// compose stack the target is the 'backend' service; a standalone
		// dev server uses the dev host port (docker-compose.dev.yml sets
		// VITE_API_PROXY_TARGET=http://backend:8080).
		proxy: {
			'/api': {
				target: process.env.VITE_API_PROXY_TARGET ?? 'http://localhost:18080',
				changeOrigin: true
			}
		}
	}
});
