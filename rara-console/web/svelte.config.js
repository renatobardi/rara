import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
export default {
	preprocess: vitePreprocess(),
	kit: {
		// Pure SPA: one index.html fallback, embedded into the Go binary and served for every
		// client route. No prerendered data — the Go BFF supplies it live at runtime.
		adapter: adapter({ fallback: 'index.html' })
	}
};
