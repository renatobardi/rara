<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		id,
		title,
		channel,
		summary,
		source_ref,
		published_at,
		ontoggle,
		expanded = false,
		actions
	}: {
		id: number;
		title?: string;
		channel?: string;
		summary?: string;
		source_ref?: string;
		published_at?: string;
		ontoggle?: () => void;
		expanded?: boolean;
		actions?: Snippet;
	} = $props();

	const pubDate = $derived(
		published_at ? new Date(published_at).toLocaleDateString('pt-BR') : ''
	);

	function isURL(s: string): boolean {
		return s.startsWith('http://') || s.startsWith('https://');
	}

	function stripHTML(s: string): string {
		return s.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
	}

	const displayTitle = $derived(title || source_ref || `#${id}`);
	const stripped = $derived(summary ? stripHTML(summary) : '');
	const hasLink = $derived(!!source_ref && isURL(source_ref));
</script>

<div class="px-4 py-3">
	<div class="flex items-start gap-2">
		<span class="min-w-0 flex-1">
			{#if hasLink}
				<a
					href={source_ref}
					target="_blank"
					rel="noopener noreferrer"
					class="block truncate text-[13.5px] font-medium hover:underline"
				>{displayTitle}</a>
			{:else}
				<span class="block truncate text-[13.5px] font-medium">{displayTitle}</span>
			{/if}
			<span class="mt-0.5 block text-[11px] text-muted">
				{channel ? `${channel} · ` : ''}{pubDate ? `${pubDate} · ` : ''}#{id}
			</span>
		</span>
		{#if ontoggle}
			<button
				class="mt-0.5 flex-none cursor-pointer border-0 bg-transparent p-0 text-[11px] text-muted opacity-50 hover:opacity-100"
				onclick={ontoggle}
				aria-expanded={expanded}
				aria-label={expanded ? 'Colapsar passos' : 'Expandir passos'}
			>{expanded ? '▲' : '▼'}</button>
		{/if}
	</div>

	{#if stripped}
		<p class="mb-0 mt-1 line-clamp-2 text-[12px] text-muted">{stripped}</p>
	{/if}

	{#if actions}
		<div class="mt-2 flex items-center justify-end">
			{@render actions()}
		</div>
	{/if}
</div>
