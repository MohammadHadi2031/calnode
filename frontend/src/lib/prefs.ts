import { writable, get } from 'svelte/store';
import type { User } from './api';
import { DateTime } from 'luxon';
import { resolveZone, zoneLabel } from './timezones';

export interface UserPrefs {
	timezone: string;
	time_format: '12h' | '24h';
	week_start: number;
	date_format: 'dmy' | 'mdy' | 'ymd';
}

const defaults: UserPrefs = {
	timezone: 'UTC',
	time_format: '12h',
	week_start: 1,
	date_format: 'dmy'
};

export const prefs = writable<UserPrefs>(defaults);

export function prefsFromUser(u: User): UserPrefs {
	return {
		timezone: u.timezone,
		time_format: u.time_format ?? '12h',
		week_start: u.week_start ?? 1,
		date_format: u.date_format ?? 'dmy'
	};
}

function zoneOf(p: UserPrefs): string {
	return resolveZone(p.timezone);
}

function fmtDatePart(dt: DateTime, format: 'dmy' | 'mdy' | 'ymd'): string {
	if (format === 'mdy') return dt.toFormat('LL/dd/yyyy');
	if (format === 'ymd') return dt.toFormat('yyyy-LL-dd');
	return dt.toFormat('dd/LL/yyyy');
}

function fmtTimePart(dt: DateTime, p: UserPrefs): string {
	return dt.toFormat(p.time_format === '24h' ? 'HH:mm' : 'h:mm a');
}

/** Date and time in the user's profile time zone (not the browser's). */
export function fmtDateTime(iso: string, p: UserPrefs = get(prefs)): string {
	const dt = DateTime.fromISO(iso).setZone(zoneOf(p));
	if (!dt.isValid) return iso;
	return `${fmtDatePart(dt, p.date_format)}, ${fmtTimePart(dt, p)}`;
}

export function fmtDate(ymd: string, p: UserPrefs = get(prefs)): string {
	// ymd is always YYYY-MM-DD from the API — parse directly to avoid TZ shift from new Date()
	const [y, m, d] = ymd.split('-');
	if (p.date_format === 'mdy') return `${m}/${d}/${y}`;
	if (p.date_format === 'ymd') return `${y}-${m}-${d}`;
	return `${d}/${m}/${y}`;
}

/** Time of day in the user's profile time zone (not the browser's). */
export function fmtTime(iso: string, p: UserPrefs = get(prefs)): string {
	const dt = DateTime.fromISO(iso).setZone(zoneOf(p));
	return dt.isValid ? fmtTimePart(dt, p) : iso;
}

/** "Asia/Tehran (UTC+03:30)": names the zone fmtDateTime and fmtTime use. */
export function fmtZone(p: UserPrefs = get(prefs)): string {
	return zoneLabel(zoneOf(p));
}

export const WEEK_DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
