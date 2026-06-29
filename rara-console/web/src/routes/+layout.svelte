<script lang="ts">
	import '../app.css';
	import { t } from '$lib/strings';
	import { page } from '$app/stores';
	import BrandMark from '$lib/BrandMark.svelte';
	import CommandPalette from '$lib/CommandPalette.svelte';

	let { children } = $props();

	let paletteOpen = $state(false);

	// Clean is the default; Dark is opt-in and persisted. The pre-paint script in app.html already
	// applied the saved choice before render; this syncs the toggle's state to it once on mount.
	let theme = $state<'clean' | 'dark'>('clean');
	$effect(() => {
		theme = document.documentElement.dataset.theme === 'dark' ? 'dark' : 'clean';
	});
	function setTheme(next: 'clean' | 'dark') {
		theme = next;
		if (next === 'dark') document.documentElement.dataset.theme = 'dark';
		else delete document.documentElement.dataset.theme;
		localStorage.setItem('theme', next);
	}

	function globalKeydown(e: KeyboardEvent) {
		if ((e.metaKey || e.ctrlKey) && e.key === 'k') {
			e.preventDefault();
			paletteOpen = true;
		}
	}

	const nav = [
		{ icon: '◍', label: t.nav.overview, href: '/' },
		{ icon: '▤', label: t.nav.pipeline, href: '/pipeline' },
		{ icon: '✦', label: t.nav.distillations, href: '/distillations' },
		{ section: t.nav.secTrain },
		{ icon: '◐', label: t.nav.curation, href: '/curadoria' },
		{ icon: '⛁', label: t.nav.fontes, href: '/fontes' },
		{ icon: '⚡', label: t.nav.workers, href: '/workers' },
		{ icon: '⊹', label: t.nav.inferencia, href: '/inferencia' },
		{ icon: '✸', label: t.nav.skills, href: '/skills' },
		{ icon: '◆', label: t.nav.agents, href: '/agents' },
		{ section: t.nav.secSystem },
		{ icon: '≣', label: t.nav.audit, href: '/auditoria' },
		{ icon: '⚙', label: t.nav.settings, href: '/configuracoes' }
	];

	const pageTitles: Record<string, string> = {
		'/': t.nav.overview,
		'/pipeline': t.nav.pipeline,
		'/distillations': t.nav.distillations,
		'/curadoria': t.nav.curation,
		'/fontes': t.nav.fontes,
		'/workers': t.nav.workers,
		'/inferencia': t.nav.inferencia,
		'/skills': t.nav.skills,
		'/agents': t.nav.agents,
		'/auditoria': t.nav.audit,
		'/configuracoes': t.settings.title
	};
</script>

<svelte:window onkeydown={globalKeydown} />

<CommandPalette bind:open={paletteOpen} />

<div class="grid h-screen grid-cols-app overflow-hidden">
	<aside class="flex flex-col gap-0.5 bg-sidebar p-3">
		<div class="flex items-center gap-2 px-2 pb-4 pt-2 text-[15px] font-semibold">
			<span
				aria-label="rara"
				class="block h-[30px] w-[30px] flex-none text-text"
			><BrandMark /></span
			>
			{t.brand}
		</div>
		<nav class="flex flex-col gap-px">
			{#each nav as it}
				{#if it.section}
					<div class="px-3 pb-1 pt-3 text-[11px] font-medium text-muted">{it.section}</div>
				{:else if it.href}
					<a
						href={it.href}
						aria-current={$page.url.pathname === it.href ? 'page' : undefined}
						class="flex items-center gap-3 rounded-token px-3 py-2 text-[13.5px] no-underline
						       {$page.url.pathname === it.href
						         ? 'bg-hover font-semibold text-text'
						         : 'text-text opacity-60 hover:bg-hover hover:opacity-90'}"
					>
						<span class="w-4 flex-none text-center opacity-70">{it.icon}</span>
						{it.label}
					</a>
				{:else}
					<!-- Shell placeholder: screen lands in C2+. Non-interactive, not announced as navigable. -->
					<span
						aria-disabled="true"
						title="Em breve"
						class="flex cursor-default items-center gap-3 rounded-token px-3 py-2 text-[13.5px] text-muted"
					>
						<span class="w-4 flex-none text-center opacity-70">{it.icon}</span>
						{it.label}
					</span>
				{/if}
			{/each}
		</nav>
		<div class="mt-auto flex items-center gap-2 px-2 py-3 text-xs text-muted">
			<span class="h-[7px] w-[7px] rounded-full bg-green"></span>
			{t.status.online}
		</div>
	</aside>

	<main class="overflow-x-hidden overflow-y-auto bg-bg">
		<div
			class="sticky top-0 z-10 flex items-center gap-4 border-b border-border px-6 py-3 backdrop-blur-md"
			style="background:color-mix(in srgb, var(--bg) 82%, transparent)"
		>
			<h1 class="m-0 text-[17px] font-semibold">{pageTitles[$page.url.pathname] ?? $page.url.pathname.slice(1)}</h1>
			<button
				class="ml-auto flex min-w-[220px] cursor-pointer items-center gap-2 rounded-pill border border-border bg-surface-2 px-3.5 py-[7px] text-[13px] text-muted"
				onclick={() => (paletteOpen = true)}
				aria-label="Abrir command palette (⌘K)"
			>
				⌕ {t.topbar.search}
				<kbd class="ml-auto text-[11px] opacity-50">⌘K</kbd>
			</button>
			<button
				class="flex h-[34px] w-[34px] cursor-pointer items-center justify-center rounded-token border-0 bg-transparent text-muted hover:bg-hover"
				onclick={() => setTheme(theme === 'dark' ? 'clean' : 'dark')}
				aria-label="Alternar tema (claro/escuro)"
			>
				{#if theme === 'dark'}
					<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
						<path d="M20 14.5A8 8 0 0 1 9.5 4 7 7 0 1 0 20 14.5Z"/>
					</svg>
				{:else}
					<svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
						<circle cx="12" cy="12" r="4"/>
						<path d="M12 2v2M12 20v2M2 12h2M20 12h2M5 5l1.4 1.4M17.6 17.6L19 19M19 5l-1.4 1.4M6.4 17.6L5 19"/>
					</svg>
				{/if}
			</button>
		</div>

		<div class="mx-auto max-w-[1180px] p-6">
			{@render children()}
		</div>
	</main>
</div>
