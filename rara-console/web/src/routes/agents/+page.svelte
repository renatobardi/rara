<script lang="ts">
	import { t } from '$lib/strings';
</script>

<div class="flex min-h-[60vh] items-center justify-center">
	<div class="max-w-md rounded-2xl border border-border bg-surface p-10 text-center shadow-sm">
		<div class="mb-4 text-4xl opacity-30" aria-hidden="true">◆</div>
		<h2 class="mb-2 text-[18px] font-semibold text-text">{t.agents.title}</h2>
		<p class="mb-5 text-[13px] font-medium uppercase tracking-widest text-muted">{t.agents.soon}</p>
		<p class="text-[13.5px] leading-relaxed text-muted">{t.agents.blurb}</p>
	</div>
</div>
