<script lang="ts">
	import type { QuarantineItem, ItemContent } from './curadoria.js';
	import { fetchItemContent } from './curadoria.js';

	export let item: QuarantineItem;

	let loading = false;
	let content: ItemContent | null = null;

	$: onItemChange(item);

	async function onItemChange(i: QuarantineItem) {
		content = null;
		if (i.lane === 'youtube') {
			content = { lane: 'youtube' };
			return;
		}
		loading = true;
		content = await fetchItemContent(i.id);
		loading = false;
	}

	function truncate(text: string, max = 800): string {
		return text.length > max ? text.slice(0, max) + '…' : text;
	}
</script>

{#if loading}
	<div class="shimmer-wrap" aria-busy="true" aria-label="Carregando prévia…">
		<div class="shimmer tall"></div>
		<div class="shimmer medium"></div>
		<div class="shimmer short"></div>
	</div>

{:else if content?.lane === 'youtube' && item.source_ref}
	<iframe
		src="https://www.youtube.com/embed/{item.source_ref}"
		title={item.title ?? 'YouTube'}
		allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture"
		allowfullscreen
		loading="lazy"
	></iframe>

{:else if content?.lane === 'email' && (content.body || content.sender)}
	<div class="content-wrap">
		{#if content.sender}
			<div class="badge">De: {content.sender}</div>
		{/if}
		{#if content.body}
			<p class="body">{truncate(content.body)}</p>
		{/if}
	</div>

{:else if content?.lane === 'news' && content.body}
	<div class="content-wrap">
		<p class="body">{truncate(content.body)}</p>
	</div>
{/if}
<!-- else: colapsa silenciosamente -->

<style>
	iframe {
		display: block;
		width: 100%;
		aspect-ratio: 16 / 9;
		border: none;
		border-radius: 0.5rem;
	}

	.content-wrap {
		height: 100%;
		overflow-y: auto;
		border: 1px solid var(--color-border, #e2e8f0);
		border-radius: 0.5rem;
		background: var(--color-surface, #fff);
	}

	.badge {
		font-size: 0.75rem;
		font-weight: 600;
		padding: 0.4rem 0.75rem;
		background: var(--color-surface-alt, #f8fafc);
		border-bottom: 1px solid var(--color-border, #e2e8f0);
		color: var(--color-text-muted, #64748b);
	}

	.body {
		margin: 0;
		padding: 0.75rem;
		font-size: 0.875rem;
		line-height: 1.6;
		white-space: pre-wrap;
		word-break: break-word;
		color: var(--color-text, #1e293b);
	}

	/* Shimmer */
	.shimmer-wrap {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
		padding: 0.75rem;
		border: 1px solid var(--color-border, #e2e8f0);
		border-radius: 0.5rem;
	}

	.shimmer {
		border-radius: 0.25rem;
		background: linear-gradient(
			90deg,
			var(--color-shimmer-base, #e2e8f0) 25%,
			var(--color-shimmer-hi, #f8fafc) 50%,
			var(--color-shimmer-base, #e2e8f0) 75%
		);
		background-size: 200% 100%;
		animation: shimmer 1.4s infinite;
	}

	.shimmer.tall {
		height: 10rem;
	}
	.shimmer.medium {
		height: 1rem;
		width: 75%;
	}
	.shimmer.short {
		height: 1rem;
		width: 50%;
	}

	@keyframes shimmer {
		0% {
			background-position: 200% 0;
		}
		100% {
			background-position: -200% 0;
		}
	}
</style>
