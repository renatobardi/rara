<script lang="ts" generics="T">
	import type { Snippet } from 'svelte';

	const PAGE_SIZES = [20, 50, 100] as const;
	type PageSize = (typeof PAGE_SIZES)[number];

	let {
		items,
		storageKey = 'rara.pageSize',
		children
	}: {
		items: T[];
		storageKey?: string;
		children: Snippet<[T[]]>;
	} = $props();

	function load(): PageSize {
		try {
			const v = Number(localStorage.getItem(storageKey));
			return (PAGE_SIZES as readonly number[]).includes(v) ? (v as PageSize) : 20;
		} catch {
			return 20;
		}
	}

	let pageSize = $state<PageSize>(load());
	let currentPage = $state(1);

	// ponytail: reset to p.1 on size change; not tracking items (safePage clamps instead)
	$effect(() => {
		void pageSize;
		currentPage = 1;
	});

	function setSize(n: PageSize) {
		pageSize = n;
		try { localStorage.setItem(storageKey, String(n)); } catch {}
	}

	const totalPages = $derived(Math.max(1, Math.ceil(items.length / pageSize)));
	const safePage = $derived(Math.min(currentPage, totalPages));
	const from = $derived((safePage - 1) * pageSize);
	const to = $derived(Math.min(from + pageSize, items.length));
	const page = $derived(items.slice(from, to));
	const show = $derived(items.length > PAGE_SIZES[0]);
</script>

{@render children(page)}

{#if show}
	<div class="flex items-center justify-between px-4 py-2 text-[11px] text-muted">
		<span class="tabular-nums">{items.length ? from + 1 : 0}–{to} de {items.length}</span>
		<div class="flex items-center gap-3">
			<span class="flex gap-0.5">
				{#each PAGE_SIZES as s}
					<button
						class="cursor-pointer rounded border-0 bg-transparent px-1.5 py-0.5 {pageSize === s
							? 'font-medium text-fg'
							: 'text-muted hover:bg-hover'}"
						onclick={() => setSize(s)}
					>{s}</button>
				{/each}
			</span>
			<span class="flex items-center gap-1 tabular-nums">
				<button
					class="cursor-pointer rounded border-0 bg-transparent px-1 py-0.5 hover:bg-hover disabled:cursor-default disabled:opacity-30"
					disabled={safePage <= 1}
					onclick={() => (currentPage = safePage - 1)}
				>‹</button>
				<span>{safePage}/{totalPages}</span>
				<button
					class="cursor-pointer rounded border-0 bg-transparent px-1 py-0.5 hover:bg-hover disabled:cursor-default disabled:opacity-30"
					disabled={safePage >= totalPages}
					onclick={() => (currentPage = safePage + 1)}
				>›</button>
			</span>
		</div>
	</div>
{/if}
