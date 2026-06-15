<script lang="ts">
	import '../app.css';
	import { t } from '$lib/strings';
	import { page } from '$app/stores';

	let { children } = $props();

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

	const nav = [
		{ icon: '◍', label: t.nav.overview, href: '/' },
		{ icon: '▤', label: t.nav.pipeline, href: '/pipeline' },
		{ icon: '⚑', label: t.nav.quarantine, href: '/quarentena' },
		{ icon: '✦', label: t.nav.distillations, href: '/distillations' },
		{ section: t.nav.secTrain },
		{ icon: '◐', label: t.nav.curation },
		{ icon: '⇄', label: t.nav.sources },
		{ icon: '⚙', label: t.nav.providers },
		{ section: t.nav.secSystem },
		{ icon: '≣', label: t.nav.audit },
		{ icon: '⚙', label: t.nav.settings }
	];

	const pageTitles: Record<string, string> = {
		'/': t.nav.overview,
		'/pipeline': t.nav.pipeline,
		'/quarentena': t.nav.quarantine,
		'/distillations': t.nav.distillations
	};
</script>

<div class="grid h-screen grid-cols-app overflow-hidden">
	<aside class="flex flex-col gap-0.5 bg-sidebar p-3">
		<div class="flex items-center gap-2 px-2 pb-4 pt-2 text-[15px] font-semibold">
			<span
				class="grid h-[26px] w-[26px] place-items-center rounded-token bg-text text-[13px] font-extrabold text-bg"
				>ra</span
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

	<main class="overflow-y-auto bg-bg">
		<div
			class="sticky top-0 z-10 flex items-center gap-4 border-b border-border px-6 py-3 backdrop-blur-md"
			style="background:color-mix(in srgb, var(--bg) 82%, transparent)"
		>
			<h1 class="m-0 text-[17px] font-semibold">{pageTitles[$page.url.pathname] ?? $page.url.pathname.slice(1)}</h1>
			<div class="flex rounded-pill border border-border bg-surface-2 p-[3px]">
				<button
					class="cursor-pointer rounded-pill border-0 px-3.5 py-1 text-xs font-semibold {theme ===
					'clean'
						? 'bg-primary text-primary-fg'
						: 'bg-transparent text-muted'}"
					onclick={() => setTheme('clean')}>{t.topbar.clean}</button
				>
				<button
					class="cursor-pointer rounded-pill border-0 px-3.5 py-1 text-xs font-semibold {theme ===
					'dark'
						? 'bg-primary text-primary-fg'
						: 'bg-transparent text-muted'}"
					onclick={() => setTheme('dark')}>{t.topbar.dark}</button
				>
			</div>
			<!-- Decorative placeholder until ⌘K lands (C4); aria-hidden so it isn't a fake search field. -->
			<div
				aria-hidden="true"
				class="ml-auto flex min-w-[220px] items-center gap-2 rounded-pill border border-border bg-surface-2 px-3.5 py-[7px] text-[13px] text-muted"
			>
				⌕ {t.topbar.search}
			</div>
		</div>

		<div class="mx-auto max-w-[1180px] p-6">
			{@render children()}
		</div>
	</main>
</div>
