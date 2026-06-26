const rtf = new Intl.RelativeTimeFormat('pt-BR', { numeric: 'auto', style: 'narrow' });

// timeAgo: relative time for an ISO timestamp ("há 5 min", "há 2 h", "há 3 dias").
// `now` is injectable so it stays pure/testable; defaults to Date.now().
// Returns '' for an unparseable timestamp.
export function timeAgo(iso: string, now: number = Date.now()): string {
	const then = new Date(iso).getTime();
	if (Number.isNaN(then)) return '';
	const sec = Math.round((then - now) / 1000); // negative = past
	if (Math.abs(sec) < 60) return rtf.format(sec, 'second');
	const min = Math.round(sec / 60);
	if (Math.abs(min) < 60) return rtf.format(min, 'minute');
	const hour = Math.round(sec / 3600);
	if (Math.abs(hour) < 24) return rtf.format(hour, 'hour');
	return rtf.format(Math.round(sec / 86400), 'day');
}
