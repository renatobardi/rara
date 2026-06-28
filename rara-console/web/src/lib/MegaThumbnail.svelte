<script lang="ts">
	import type { QuarantineItem, ItemContent, PreviewResult } from './curadoria.js';
	import { fetchItemContent, fetchPreview } from './curadoria.js';

	export let item: QuarantineItem;

	let loading = false;
	let content: ItemContent | null = null;  // email lane
	let preview: PreviewResult | null = null; // news lane

	$: onItemChange(item);

	async function onItemChange(i: QuarantineItem) {
		content = null;
		preview = null;
		if (i.lane === 'youtube') {
			loading = false;
			content = { lane: 'youtube' };
			return;
		}
		loading = true;
		const token = `${i.id}:${i.lane}:${i.source_ref ?? ''}`;
		try {
			if (i.lane === 'news' && i.source_ref) {
				const result = await fetchPreview(i.source_ref);
				if (`${item.id}:${item.lane}:${item.source_ref ?? ''}` === token) preview = result;
			} else {
				const result = await fetchItemContent(i.id);
				if (`${item.id}:${item.lane}:${item.source_ref ?? ''}` === token) content = result;
			}
		} finally {
			if (`${item.id}:${item.lane}:${item.source_ref ?? ''}` === token) loading = false;
		}
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
		src="https://www.youtube-nocookie.com/embed/{item.source_ref}"
		title={item.title ?? 'YouTube'}
		allow="accelerometer; autoplay; clipboard-write; encrypted-media; gyroscope; picture-in-picture"
		allowfullscreen
		loading="lazy"
	></iframe>

{:else if preview?.embeddable && preview.url}
	<iframe
		src={preview.url}
		title={item.title ?? 'Prévia'}
		sandbox="allow-scripts"
		loading="lazy"
	></iframe>

{:else if preview && preview.image_url}
	<div class="og-wrap">
		<img src={preview.image_url} alt={item.title ?? ''} loading="lazy" />
	</div>

{:else if content?.lane === 'email' && (content.body || content.sender)}
	<div class="content-wrap">
		{#if content.sender}
			<div class="badge">De: {content.sender}</div>
		{/if}
		{#if content.body}
			<p class="body">{truncate(content.body)}</p>
		{/if}
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

	.og-wrap {
		width: 100%;
		border-radius: 0.5rem;
		overflow: hidden;
	}

	.og-wrap img {
		display: block;
		width: 100%;
		height: auto;
		object-fit: cover;
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
