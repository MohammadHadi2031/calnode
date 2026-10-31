// Time zone lists and labels for the admin UI (wisegrid fork). Every place that shows a
// time also says which zone it is in, and every zone picker offers the full IANA list.

import { DateTime, IANAZone } from 'luxon';

/** A short fallback list, only for browsers without Intl.supportedValuesOf. */
const FALLBACK_ZONES = [
	'Pacific/Auckland', 'Australia/Sydney', 'Australia/Melbourne', 'Asia/Tokyo', 'Asia/Shanghai',
	'Asia/Singapore', 'Asia/Kolkata', 'Asia/Karachi', 'Asia/Dubai', 'Asia/Tehran', 'Asia/Riyadh',
	'Europe/Istanbul', 'Asia/Amman', 'Europe/Moscow', 'Africa/Cairo', 'Europe/Athens', 'Europe/Berlin',
	'Europe/Paris', 'Europe/Amsterdam', 'Europe/London', 'America/Sao_Paulo', 'America/New_York',
	'America/Chicago', 'America/Denver', 'America/Los_Angeles', 'Pacific/Honolulu', 'UTC'
];

// Chrome still lists some zones under their pre-rename IANA names, so a search for
// "Kolkata" or "Kyiv" found nothing. Offer the current name; Go's tzdata knows both.
const RENAMED: Record<string, string> = {
	'Asia/Calcutta': 'Asia/Kolkata',
	'Europe/Kiev': 'Europe/Kyiv',
	'Asia/Saigon': 'Asia/Ho_Chi_Minh',
	'Asia/Katmandu': 'Asia/Kathmandu',
	'Asia/Rangoon': 'Asia/Yangon',
	'America/Godthab': 'America/Nuuk',
	'Atlantic/Faeroe': 'Atlantic/Faroe',
	'Pacific/Enderbury': 'Pacific/Kanton',
	'Pacific/Ponape': 'Pacific/Pohnpei',
	'Pacific/Truk': 'Pacific/Chuuk',
	'America/Buenos_Aires': 'America/Argentina/Buenos_Aires',
	'America/Indianapolis': 'America/Indiana/Indianapolis',
	'America/Louisville': 'America/Kentucky/Louisville'
};

let cached: string[] | null = null;

/** Every IANA zone the browser knows (under current names), plus UTC. */
export function allTimeZones(): string[] {
	if (cached) return cached;
	let list: string[] = [];
	try {
		list = (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.('timeZone') ?? [];
	} catch {
		/* older browser */
	}
	if (list.length === 0) list = FALLBACK_ZONES;
	const zones = new Set(list.map((z) => (RENAMED[z] && IANAZone.isValidZone(RENAMED[z]) ? RENAMED[z] : z)));
	zones.add('UTC');
	cached = [...zones];
	return cached;
}

/** The zone if valid, else the browser's, else UTC. */
export function resolveZone(z: string | undefined | null): string {
	if (z && IANAZone.isValidZone(z)) return z;
	return Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
}

/** "UTC+03:30", "UTC-04:00", "UTC" at the given instant (DST-aware). */
export function zoneOffset(zone: string, at: DateTime = DateTime.now()): string {
	const off = at.setZone(zone).offset;
	if (off === 0) return 'UTC';
	const sign = off > 0 ? '+' : '-';
	const abs = Math.abs(off);
	return `UTC${sign}${String(Math.floor(abs / 60)).padStart(2, '0')}:${String(abs % 60).padStart(2, '0')}`;
}

/** The city part of an IANA name: "Asia/Tehran" → "Tehran", "America/Argentina/Buenos_Aires" → "Buenos Aires". */
export function zoneCity(zone: string): string {
	if (zone === 'UTC' || zone === 'Etc/UTC') return 'UTC';
	return (zone.split('/').pop() ?? zone).replace(/_/g, ' ');
}

/** "Asia/Tehran (UTC+03:30)": the label used wherever a time's zone is named. */
export function zoneLabel(zone: string, at?: DateTime): string {
	const off = zoneOffset(zone, at);
	return zone === 'UTC' ? 'UTC' : `${zone.replace(/_/g, ' ')} (${off})`;
}

export type ZoneItem = { value: string; label: string };

/**
 * Combobox items for every zone, ordered by current UTC offset then name, labelled
 * "(UTC+03:30) Asia/Tehran" so a search matches the city, the region or the offset.
 * `include` adds zones the list may lack (e.g. a stored legacy name) so they stay selectable.
 */
export function zoneItems(include: (string | undefined | null)[] = []): ZoneItem[] {
	const now = DateTime.now();
	const zones = new Set(allTimeZones());
	for (const z of include) if (z && IANAZone.isValidZone(z)) zones.add(z);
	return [...zones]
		.map((z) => ({ z, off: now.setZone(z).offset }))
		.sort((a, b) => a.off - b.off || a.z.localeCompare(b.z))
		.map(({ z }) => ({ value: z, label: `(${zoneOffset(z, now)}) ${z.replace(/_/g, ' ')}` }));
}
