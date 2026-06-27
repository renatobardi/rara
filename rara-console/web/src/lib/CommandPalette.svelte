<script lang="ts">
	import { t } from '$lib/strings';
	import { goto } from '$app/navigation';

	type Item = { label: string; href: string };

	const items: Item[] = [
		{ label: t.nav.overview, href: '/' },
		{ label: t.nav.pipeline, href: '/pipeline' },
		{ label: t.nav.distillations, href: '/distillations' },
		{ label: t.nav.curation, href: '/curadoria' },
		{ label: t.nav.workers, href: '/workers' },
		{ label: t.nav.agents, href: '/agents' },
		{ label: t.nav.audit, href: '/auditoria' }
	];

	let { open = $bindable(false) }: { open: boolean } = $props();

	let query = $state('');
	let activeIdx = $state(0);

	const filtered = $derived(
		query.trim() === ''
			? items
			: items.filter((it) => it.label.toLowerCase().includes(query.toLowerCase()))
	);

	$effect(() => {
		if (!open) {
			query = '';
			activeIdx = 0;
		}
	});

	// Reset selection whenever the filtered list changes (e.g. user types a query that shrinks
	// the list below the current activeIdx — Enter would otherwise hit undefined).
	$effect(() => {
		filtered;
		activeIdx = 0;
	});

	function pick(href: string) {
		open = false;
		goto(href);
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			activeIdx = (activeIdx + 1) % filtered.length;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			activeIdx = (activeIdx - 1 + filtered.length) % filtered.length;
		} else if (e.key === 'Enter' && filtered[activeIdx]) {
			pick(filtered[activeIdx].href);
		} else if (e.key === 'Escape') {
			open = false;
		}
	}
</script>

{#if open}
	<!-- svelte-ignore a11y_click_events_have_key_events -->
	<div
		role="presentation"
		class="fixed inset-0 z-50 flex items-start justify-center pt-[15vh]"
		style="background:rgba(0,0,0,0.35)"
		onclick={(e) => { if (e.target === e.currentTarget) open = false; }}
	>
		<div
			class="w-full max-w-md rounded-xl border border-border bg-bg shadow-2xl"
			role="dialog"
			aria-label="Command palette"
		>
			<!-- svelte-ignore a11y_autofocus -->
			<input
				class="w-full rounded-t-xl border-0 border-b border-border bg-transparent px-4 py-3 text-[14px] text-text outline-none placeholder:text-muted"
				placeholder={t.cmdPalette.placeholder}
				bind:value={query}
				onkeydown={onKeydown}
				autofocus
			/>
			<ul class="max-h-72 overflow-y-auto py-1" role="listbox">
				{#if filtered.length === 0}
					<li class="px-4 py-3 text-[13px] text-muted">{t.cmdPalette.noResults}</li>
				{:else}
					{#each filtered as it, i}
						<!-- svelte-ignore a11y_click_events_have_key_events -->
						<li
							role="option"
							aria-selected={i === activeIdx}
							class="cursor-pointer px-4 py-2.5 text-[13px] {i === activeIdx
								? 'bg-hover text-text'
								: 'text-text opacity-70 hover:bg-hover hover:opacity-100'}"
							onclick={() => pick(it.href)}
						>
							{it.label}
						</li>
					{/each}
				{/if}
			</ul>
		</div>
	</div>
{/if}
