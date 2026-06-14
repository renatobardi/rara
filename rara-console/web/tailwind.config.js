/**
 * Option B design tokens (CONSOLE-PLAN §0) as a Tailwind preset. Colors resolve to CSS variables
 * (defined in app.css for Clean + Dark), so theme switching is a runtime attribute flip, not a
 * rebuild. Radius 14 (cards) / 999 (pills), sidebar 268px, font system-ui.
 * @type {import('tailwindcss').Config}
 */
export default {
	content: ['./src/**/*.{html,svelte,js,ts}'],
	darkMode: ['selector', '[data-theme="dark"]'],
	theme: {
		extend: {
			colors: {
				bg: 'var(--bg)',
				sidebar: 'var(--sidebar)',
				surface: 'var(--surface)',
				'surface-2': 'var(--surface-2)',
				border: 'var(--border)',
				hover: 'var(--hover)',
				text: 'var(--text)',
				muted: 'var(--muted)',
				primary: 'var(--primary)',
				'primary-fg': 'var(--primary-fg)',
				green: 'var(--green)',
				blue: 'var(--blue)',
				violet: 'var(--violet)',
				amber: 'var(--amber)',
				red: 'var(--red)'
			},
			borderRadius: { card: '14px', token: '10px', pill: '999px' },
			fontFamily: { sans: 'var(--font)' },
			width: { sidebar: '268px' },
			gridTemplateColumns: { app: '268px 1fr' }
		}
	}
};
