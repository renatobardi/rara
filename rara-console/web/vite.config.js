import { sveltekit } from '@sveltejs/kit/vite';

export default {
	plugins: [sveltekit()],
	// Dev-only: proxy the BFF to a locally running console (or core) so `vite dev` talks to a real
	// surface. In production the Go binary serves both the SPA and /api, so no proxy is involved.
	server: {
		proxy: {
			'/api': 'http://127.0.0.1:8081',
			'/healthz': 'http://127.0.0.1:8081'
		}
	}
};
