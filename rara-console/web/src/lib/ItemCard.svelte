<script lang="ts">
	import type { Snippet } from 'svelte';

	let {
		id,
		title,
		channel,
		summary,
		source_ref,
		ontoggle,
		expanded = false,
		actions
	}: {
		id: number;
		title?: string;
		channel?: string;
		summary?: string;
		source_ref?: string;
		ontoggle?: () => void;
		expanded?: boolean;
		actions?: Snippet;
	} = $props();

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

<!-- ponytail: div+onclick avoids nested <a> inside <button>; stopPropagation on link handles overlap -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<div
	class="px-4 py-3{ontoggle ? ' cursor-pointer hover:bg-hover' : ''}"
	onclick={ontoggle}
	role={ontoggle ? 'button' : undefined}
	tabindex={ontoggle ? 0 : undefined}
	onkeydown={ontoggle ? (e) => (e.key === 'Enter' || e.key === ' ') && ontoggle() : undefined}
>
	<div class="flex items-start gap-2">
		<span class="min-w-0 flex-1">
			<span class="block truncate text-[13.5px] font-medium">{displayTitle}</span>
			<span class="mt-0.5 block text-[11px] text-muted">
				{channel ? `${channel} ` : ''}#{id}
			</span>
		</span>
		{#if ontoggle}
			<span class="mt-0.5 flex-none text-[11px] text-muted opacity-50">{expanded ? '▲' : '▼'}</span>
		{/if}
	</div>

	{#if stripped}
		<p class="mb-0 mt-1 line-clamp-2 text-[12px] text-muted">{stripped}</p>
	{/if}

	{#if hasLink || actions}
		<div class="mt-2 flex items-center gap-4">
			<span class="flex-1">
				{#if hasLink}
					<a
						href={source_ref}
						target="_blank"
						rel="noopener noreferrer"
						class="text-[11px] text-muted underline hover:text-fg"
						onclick={(e) => e.stopPropagation()}
					>abrir</a>
				{/if}
			</span>
			{#if actions}
				{@render actions()}
			{/if}
		</div>
	{/if}
</div>
