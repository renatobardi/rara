<script lang="ts">
	import { page } from '$app/stores';
	import { t } from '$lib/strings';

	type Distillation = {
		id: number;
		source_type: string;
		source_ref: string;
		title?: string;
		doc_context?: string;
		engine: string;
		status: string;
		content?: string;
		pattern: string;
		created_at: string;
	};

	let d = $state<Distillation | null>(null);
	let loading = $state(true);
	let error = $state(false);

	let feedbackState = $state<'idle' | 'pending' | 'done' | 'err'>('idle');

	$effect(() => {
		const id = $page.params.id;
		fetch(`/api/distillations/${id}`)
			.then((r) => (r.ok ? r.json() : Promise.reject(r.status)))
			.then((data) => (d = data))
			.catch(() => (error = true))
			.finally(() => (loading = false));
	});

	async function sendFeedback(signal: 'up' | 'down') {
		if (!d || feedbackState === 'pending') return;
		feedbackState = 'pending';
		try {
			const r = await fetch('/api/feedback/distillation', {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ distillation_id: String(d.id), signal })
			});
			feedbackState = r.ok ? 'done' : 'err';
		} catch {
			feedbackState = 'err';
		}
	}

	// Minimal markdown renderer: handles headers, bold, inline code, code blocks, paragraphs.
	// No lib needed — distillation content is LLM-structured markdown, not arbitrary HTML.
	function renderMd(text: string): string {
		const lines = text.split('\n');
		const out: string[] = [];
		let inCode = false;

		for (const raw of lines) {
			if (raw.startsWith('```')) {
				if (inCode) {
					out.push('</code></pre>');
					inCode = false;
				} else {
					out.push('<pre class="overflow-x-auto rounded bg-surface-2 p-3 text-[12px]"><code>');
					inCode = true;
				}
				continue;
			}
			if (inCode) {
				out.push(esc(raw));
				continue;
			}
			if (raw.startsWith('### ')) {
				out.push(`<h4 class="mb-1 mt-4 text-[14px] font-semibold">${esc(raw.slice(4))}</h4>`);
			} else if (raw.startsWith('## ')) {
				out.push(`<h3 class="mb-1 mt-5 text-[15px] font-semibold">${esc(raw.slice(3))}</h3>`);
			} else if (raw.startsWith('# ')) {
				out.push(`<h2 class="mb-2 mt-6 text-[17px] font-semibold">${esc(raw.slice(2))}</h2>`);
			} else if (raw.trim() === '') {
				out.push('<div class="h-3"></div>');
			} else {
				out.push(`<p class="m-0 text-[13.5px] leading-relaxed">${inline(raw)}</p>`);
			}
		}
		if (inCode) out.push('</code></pre>');
		return out.join('\n');
	}

	function esc(s: string): string {
		return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
	}

	function inline(s: string): string {
		return esc(s)
			.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
			.replace(/\*([^*]+)\*/g, '<em>$1</em>')
			.replace(/`([^`]+)`/g, '<code class="rounded bg-surface-2 px-1 text-[12px]">$1</code>');
	}
</script>

<div class="mb-4">
	<a href="/distillations" class="text-[13px] text-muted no-underline hover:text-text">
		{t.distillations.back}
	</a>
</div>

{#if loading}
	<p class="text-muted">{t.distillations.detailLoading}</p>
{:else if error || !d}
	<p class="text-red">{t.distillations.detailError}</p>
{:else}
	<!-- header -->
	<div class="mb-5 flex items-start justify-between gap-4">
		<div class="flex flex-col gap-1">
			<h2 class="m-0 text-[20px] font-semibold">
				{d.title ?? `${d.source_type} · ${d.source_ref}`}
			</h2>
			<div class="flex gap-3 text-[12px] text-muted">
				<span>{d.engine}</span>
				<span>·</span>
				<span>{d.source_type} / {d.source_ref}</span>
				<span>·</span>
				<span>{d.status}</span>
			</div>
		</div>

		<!-- thumbs -->
		<div class="flex shrink-0 items-center gap-2">
			{#if feedbackState === 'done'}
				<span class="text-[12px] text-muted">{t.distillations.feedbackSent}</span>
			{:else if feedbackState === 'err'}
				<div class="flex items-center gap-2">
					<span class="text-[12px] text-red">{t.distillations.feedbackError}</span>
					<button
						class="cursor-pointer rounded-token border border-border bg-transparent px-2 py-0.5 text-[11px] hover:bg-hover"
						onclick={() => (feedbackState = 'idle')}
					>{t.distillations.retry}</button>
				</div>
			{:else}
				<button
					class="cursor-pointer rounded-token border border-border bg-transparent px-3 py-1.5 text-[16px] hover:bg-hover disabled:opacity-50"
					disabled={feedbackState === 'pending'}
					onclick={() => sendFeedback('up')}
					aria-label="Gostei"
				>
					{t.distillations.thumbUp}
				</button>
				<button
					class="cursor-pointer rounded-token border border-border bg-transparent px-3 py-1.5 text-[16px] hover:bg-hover disabled:opacity-50"
					disabled={feedbackState === 'pending'}
					onclick={() => sendFeedback('down')}
					aria-label="Não gostei"
				>
					{t.distillations.thumbDown}
				</button>
			{/if}
		</div>
	</div>

	{#if d.doc_context}
		<p class="mb-5 text-[13px] text-muted">{d.doc_context}</p>
	{/if}

	<!-- content -->
	{#if d.content}
		<div class="rounded-card border border-border bg-surface p-5">
			<!-- ponytail: @html is safe — renderMd escapes all user content before inserting markup -->
			{@html renderMd(d.content)}
		</div>
	{:else}
		<p class="text-[13px] text-muted">Sem conteúdo.</p>
	{/if}
{/if}
