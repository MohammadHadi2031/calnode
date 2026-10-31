<script lang="ts">
	import { onMount, tick } from 'svelte';
	import { Calendar } from '@fullcalendar/core';
	import type {
		DateSelectArg,
		EventClickArg,
		EventDropArg,
		EventInput,
		EventMountArg,
		EventSourceFuncArg,
		FormatterInput
	} from '@fullcalendar/core';
	import timeGridPlugin from '@fullcalendar/timegrid';
	import dayGridPlugin from '@fullcalendar/daygrid';
	import interactionPlugin, { type EventResizeDoneArg } from '@fullcalendar/interaction';
	import luxonPlugin from '@fullcalendar/luxon3';
	import { DateTime, IANAZone } from 'luxon';
	import {
		api,
		conflictsOf,
		type BusyError,
		type Conflict,
		type StaffBooking,
		type StaffCalendar,
		type StaffHost,
		type UnavailableBlock
	} from '$lib/api';
	import { currentUser } from '$lib/stores';
	import { resolveZone, zoneItems, zoneLabel, zoneOffset } from '$lib/timezones';
	import { Button, buttonVariants } from '$lib/components/ui/button';
	import { Checkbox } from '$lib/components/ui/checkbox';
	import { Combobox } from '$lib/components/ui/combobox';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import * as Dialog from '$lib/components/ui/dialog';
	import * as Popover from '$lib/components/ui/popover';
	import * as Select from '$lib/components/ui/select';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { Switch } from '$lib/components/ui/switch';
	import { toast } from 'svelte-sonner';
	import GlobeIcon from '@lucide/svelte/icons/globe';
	import PlusIcon from '@lucide/svelte/icons/plus';
	import XIcon from '@lucide/svelte/icons/x';

	// ---------------------------------------------------------------------------------
	// Viewer preferences. The calendar renders in the viewer's profile time zone (or one
	// they switch the view to), never silently in the browser's, so every conversion below
	// goes through luxon with an explicit zone, and the zone is always on screen.
	// ---------------------------------------------------------------------------------

	const profileTz = resolveZone($currentUser?.timezone);
	const tzStorageKey = `calnode.schedule.tz.${$currentUser?.id ?? 'anon'}`;
	function restoreViewTz(): string {
		try {
			const z = localStorage.getItem(tzStorageKey);
			if (z && IANAZone.isValidZone(z)) return z;
		} catch {
			/* storage unavailable */
		}
		return profileTz;
	}
	/** The zone the calendar is drawn in. */
	const initialViewTz = restoreViewTz();
	let viewTz = $state(initialViewTz);
	const ZONE_ITEMS = zoneItems([profileTz, initialViewTz]);

	function setViewTz(z: string) {
		if (!z || !IANAZone.isValidZone(z) || z === viewTz) return;
		viewTz = z;
		try {
			if (z === profileTz) localStorage.removeItem(tzStorageKey);
			else localStorage.setItem(tzStorageKey, z);
		} catch {
			/* private mode etc. */
		}
		calendar?.setOption('timeZone', z);
	}

	const hour12 = ($currentUser?.time_format ?? '12h') !== '24h';
	const weekStart = $currentUser?.week_start ?? 1;
	const dateFormat = $currentUser?.date_format ?? 'dmy';

	const DATE_PATTERN = dateFormat === 'mdy' ? 'LL/dd/yyyy' : dateFormat === 'ymd' ? 'yyyy-LL-dd' : 'dd/LL/yyyy';
	const TIME_PATTERN = hour12 ? 'h:mm a' : 'HH:mm';
	const INPUT_PATTERN = "yyyy-LL-dd'T'HH:mm"; // <input type="datetime-local"> value

	const fcTimeFormat: FormatterInput = hour12
		? { hour: 'numeric', minute: '2-digit', omitZeroMinute: true, meridiem: 'short' }
		: { hour: '2-digit', minute: '2-digit', hourCycle: 'h23' };

	/** A FullCalendar date string (offset included with the luxon plugin) as a DateTime in the view zone. */
	function fromFc(s: string): DateTime {
		return DateTime.fromISO(s, { zone: viewTz, setZone: true }).setZone(viewTz);
	}
	function toUtcIso(dt: DateTime): string {
		return dt.toUTC().toISO({ suppressMilliseconds: true }) ?? '';
	}
	/** <input type="datetime-local"> value: the instant's wall time in zone. */
	function toInput(dt: DateTime, zone: string): string {
		return dt.setZone(zone).toFormat(INPUT_PATTERN);
	}
	/** The instant an <input type="datetime-local"> value means in zone. */
	function fromInput(v: string, zone: string): DateTime | null {
		if (!v) return null;
		const d = DateTime.fromISO(v, { zone });
		return d.isValid ? d : null;
	}
	function fmtTimeOf(iso: string): string {
		return DateTime.fromISO(iso).setZone(viewTz).toFormat(TIME_PATTERN);
	}
	/** "Thu 29/10/2026, 10:00 AM – 10:30 AM" in zone (without naming the zone). */
	function fmtRange(start: DateTime | string, end: DateTime | string, zone: string = viewTz): string {
		const s = (typeof start === 'string' ? DateTime.fromISO(start) : start).setZone(zone);
		const e = (typeof end === 'string' ? DateTime.fromISO(end) : end).setZone(zone);
		const day = (d: DateTime) => `${d.toFormat('ccc')} ${d.toFormat(DATE_PATTERN)}`;
		if (s.hasSame(e, 'day')) return `${day(s)}, ${s.toFormat(TIME_PATTERN)} – ${e.toFormat(TIME_PATTERN)}`;
		return `${day(s)}, ${s.toFormat(TIME_PATTERN)} – ${day(e)}, ${e.toFormat(TIME_PATTERN)}`;
	}

	/** One line per distinct zone: who is in it, and the range in their local time. */
	type ZoneRow = { who: string; zone: string; when: string; label: string };
	function zoneRows(s: DateTime, e: DateTime, parties: { who: string; zone?: string | null }[]): ZoneRow[] {
		const rows: { who: string[]; zone: string }[] = [];
		for (const p of parties) {
			if (!p.zone || !IANAZone.isValidZone(p.zone)) continue;
			const r = rows.find((x) => x.zone === p.zone);
			if (!r) rows.push({ who: [p.who], zone: p.zone });
			else if (!r.who.includes(p.who)) r.who.push(p.who);
		}
		return rows.map((r) => ({ who: r.who.join(' & '), zone: r.zone, when: fmtRange(s, e, r.zone), label: zoneLabel(r.zone, s) }));
	}
	/** The grid corner's two-line offset ("UTC" over "+03:30"); \A is a CSS line break. */
	function cornerLabel(zone: string): string {
		const off = zoneOffset(zone);
		return off === 'UTC' ? 'UTC' : `UTC\\A${off.slice(3)}`;
	}
	/** Who the calendar's zone stands for in those rows. */
	const viewerWho = $derived(viewTz === profileTz ? 'You' : 'Schedule view');

	// ---------------------------------------------------------------------------------
	// Hosts and selection
	// ---------------------------------------------------------------------------------

	let hosts = $state<StaffHost[]>([]);
	let selectedHostIds = $state<string[]>([]);
	let loadingHosts = $state(true);
	let hostsError = $state('');
	let fetchError = $state('');
	let busyErrors = $state<BusyError[]>([]);
	let busyErrorsDismissed = $state(false);

	// Colour-blind-friendly-ish, readable with white text in both themes.
	const PALETTE = ['#2563eb', '#059669', '#d97706', '#7c3aed', '#e11d48', '#0891b2', '#ea580c', '#65a30d', '#c026d3', '#0d9488', '#4f46e5', '#b45309'];

	// Stable per host: the colour follows the host's position in the server's list, which
	// is ordered independently of the selection, so toggling hosts never recolours anyone.
	const colorIndex = $derived(new Map(hosts.map((h, i) => [h.id, i % PALETTE.length])));
	function hostColor(id: string): string {
		const i = colorIndex.get(id);
		if (i !== undefined) return PALETTE[i];
		// A co-host the caller can't manage: hash the id so it is at least consistent.
		let h = 0;
		for (const c of id) h = (h * 31 + c.charCodeAt(0)) >>> 0;
		return PALETTE[h % PALETTE.length];
	}
	function hostName(id: string): string {
		return hosts.find((h) => h.id === id)?.name ?? 'Unknown host';
	}

	const multiHost = $derived(hosts.length > 1);
	const storageKey = $derived(`calnode.schedule.hosts.${$currentUser?.id ?? 'anon'}`);

	function defaultSelection(list: StaffHost[]): string[] {
		const me = list.find((h) => h.self);
		if (me && me.event_types.length > 0) return [me.id];
		return list.map((h) => h.id);
	}

	function restoreSelection(list: StaffHost[]): string[] {
		try {
			const raw = localStorage.getItem(storageKey);
			if (raw) {
				const saved = JSON.parse(raw);
				if (Array.isArray(saved)) {
					const valid = saved.filter((id) => typeof id === 'string' && list.some((h) => h.id === id));
					if (valid.length > 0) return valid;
				}
			}
		} catch {
			/* storage unavailable or corrupt: fall back to the default */
		}
		return defaultSelection(list);
	}

	function setSelection(ids: string[]) {
		// Keep the server's order so the request and the legend are deterministic.
		selectedHostIds = hosts.filter((h) => ids.includes(h.id)).map((h) => h.id);
		try {
			localStorage.setItem(storageKey, JSON.stringify(selectedHostIds));
		} catch {
			/* private mode etc. */
		}
		calendar?.refetchEvents();
	}

	function toggleHost(id: string, on: boolean) {
		const next = on ? [...selectedHostIds, id] : selectedHostIds.filter((x) => x !== id);
		setSelection(next);
	}

	const selectionLabel = $derived.by(() => {
		if (selectedHostIds.length === 0) return 'No hosts';
		if (selectedHostIds.length === hosts.length) return 'All hosts';
		if (selectedHostIds.length === 1) return hostName(selectedHostIds[0]);
		return `${selectedHostIds.length} hosts`;
	});

	// ---------------------------------------------------------------------------------
	// Calendar
	// ---------------------------------------------------------------------------------

	let calendarEl: HTMLDivElement | undefined = $state();
	let calendar: Calendar | null = null;

	type BookingProps = { kind: 'booking'; booking: StaffBooking };
	type BlockProps = { kind: 'block'; block: UnavailableBlock };
	type BusyProps = { kind: 'busy'; host_id: string };
	type ItemProps = BookingProps | BlockProps | BusyProps;

	async function fetchEvents(info: EventSourceFuncArg): Promise<EventInput[]> {
		if (selectedHostIds.length === 0) {
			busyErrors = [];
			return [];
		}
		const p = new URLSearchParams({
			from: toUtcIso(fromFc(info.startStr)),
			to: toUtcIso(fromFc(info.endStr)),
			host_ids: selectedHostIds.join(','),
			external: '1'
		});
		try {
			const res = await api.get<StaffCalendar>(`/v1/staff/calendar?${p}`);
			fetchError = '';
			const errs = res.busy_errors ?? [];
			if (errs.map((e) => e.host_id + e.error).join() !== busyErrors.map((e) => e.host_id + e.error).join()) {
				busyErrorsDismissed = false;
			}
			busyErrors = errs;
			return toEvents(res);
		} catch (e: any) {
			fetchError = e?.message || 'Could not load the calendar';
			throw e;
		}
	}

	function toEvents(res: StaffCalendar): EventInput[] {
		const out: EventInput[] = [];
		for (const b of res.bookings ?? []) {
			if (b.status === 'cancelled') continue;
			const color = hostColor(b.primary_host_id);
			out.push({
				id: `b:${b.id}`,
				title: `${b.attendee_name || b.attendee_email || 'Attendee'} · ${b.event_type_name}`,
				start: b.start_at,
				end: b.end_at,
				backgroundColor: color,
				borderColor: color,
				textColor: '#ffffff',
				editable: b.can_manage,
				classNames: ['cn-booking', b.can_manage ? 'cn-manageable' : 'cn-readonly'],
				extendedProps: { kind: 'booking', booking: b } satisfies BookingProps
			});
		}
		for (const u of res.unavailable ?? []) {
			out.push({
				id: `u:${u.id}`,
				title: u.title || 'Unavailable',
				start: u.start_at,
				end: u.end_at,
				// Background and text come from the hatched .cn-block style; only the edge
				// carries the host colour so blocks of several hosts stay distinguishable.
				borderColor: hostColor(u.host_id),
				editable: true,
				classNames: ['cn-block'],
				extendedProps: { kind: 'block', block: u } satisfies BlockProps
			});
		}
		for (const x of res.busy ?? []) {
			out.push({
				title: 'Busy (calendar)',
				start: x.start_at,
				end: x.end_at,
				display: 'background',
				backgroundColor: multiHost ? hostColor(x.host_id) : undefined,
				editable: false,
				classNames: ['cn-busy'],
				extendedProps: { kind: 'busy', host_id: x.host_id } satisfies BusyProps
			});
		}
		return out;
	}

	function onEventDidMount(arg: EventMountArg) {
		// The preview FullCalendar draws while a range is dragged has no props. Reading a
		// booking off it threw, which froze the drag-select at its first slot.
		const p = arg.event.extendedProps as ItemProps;
		if (arg.isMirror || !p?.kind) return;
		const suffix = (id: string) => (multiHost ? ` · ${hostName(id)}` : '');
		if (p.kind === 'busy') {
			arg.el.title = `Busy (calendar)${suffix(p.host_id)}`;
		} else if (p.kind === 'block') {
			arg.el.title = `${p.block.title || 'Unavailable'}${suffix(p.block.host_id)}`;
		} else {
			const b = p.booking;
			arg.el.title = `${arg.event.title}\n${fmtTimeOf(b.start_at)} – ${fmtTimeOf(b.end_at)}${suffix(b.primary_host_id)}`;
		}
	}

	function refetch() {
		calendar?.refetchEvents();
	}

	onMount(() => {
		let destroyed = false;
		(async () => {
			try {
				const res = await api.get<{ hosts: StaffHost[] }>('/v1/staff/hosts');
				if (destroyed) return;
				hosts = res.hosts ?? [];
				selectedHostIds = restoreSelection(hosts);
			} catch (e: any) {
				if (destroyed) return;
				hostsError = e?.message || 'Could not load hosts';
			} finally {
				loadingHosts = false;
			}
			// The container was hidden while loading; let it lay out before FullCalendar measures it.
			await tick();
			if (destroyed || !calendarEl || hostsError) return;
			const narrow = window.matchMedia('(max-width: 640px)').matches;
			calendar = new Calendar(calendarEl, {
				plugins: [timeGridPlugin, dayGridPlugin, interactionPlugin, luxonPlugin],
				timeZone: viewTz,
				initialView: narrow ? 'timeGridDay' : 'timeGridWeek',
				firstDay: weekStart,
				headerToolbar: {
					left: 'prev,next today',
					center: 'title',
					right: 'timeGridDay,timeGridWeek,dayGridMonth'
				},
				buttonText: { today: 'Today', day: 'Day', week: 'Week', month: 'Month' },
				height: '100%',
				allDaySlot: false,
				nowIndicator: true,
				scrollTime: '08:00:00',
				slotDuration: '00:30:00',
				slotLabelFormat: fcTimeFormat,
				eventTimeFormat: fcTimeFormat,
				dayMaxEvents: true,
				stickyHeaderDates: true,
				selectable: true,
				selectMirror: true,
				editable: false, // per event: bookings with can_manage, and every block
				longPressDelay: 400,
				events: fetchEvents,
				eventDidMount: onEventDidMount,
				select: onSelect,
				eventClick: onEventClick,
				eventDrop: (info) => onEventChange(info),
				eventResize: (info) => onEventChange(info),
				eventSourceFailure: (e) => {
					fetchError = e?.message || 'Could not load the calendar';
				}
			});
			calendar.render();
		})();
		return () => {
			destroyed = true;
			calendar?.destroy();
			calendar = null;
		};
	});

	// ---------------------------------------------------------------------------------
	// Conflicts
	// ---------------------------------------------------------------------------------

	// These can't be overridden with force: staff can't book into the past, and two
	// appointments for one host can't start at the same instant.
	const BLOCKING_KINDS = new Set(['past', 'same_start']);

	const CONFLICT_LABELS: Record<string, string> = {
		past: 'the time is in the past',
		same_start: 'another appointment starts at exactly this time',
		outside_hours: 'outside working hours',
		booking: 'overlaps another booking',
		unavailable: 'during an unavailable block',
		calendar: 'busy on their calendar',
		calendar_unchecked: 'calendar could not be checked'
	};
	function conflictLabel(c: Conflict): string {
		return CONFLICT_LABELS[c.kind] ?? c.kind.replace(/_/g, ' ');
	}
	function isBlocking(list: Conflict[] | null): boolean {
		return !!list && list.some((c) => BLOCKING_KINDS.has(c.kind));
	}

	// ---------------------------------------------------------------------------------
	// Create dialog (drag-select)
	// ---------------------------------------------------------------------------------

	type Mode = 'appointment' | 'unavailable';

	let createOpen = $state(false);
	let cMode = $state<Mode>('appointment');
	let cStart = $state('');
	let cEnd = $state('');
	/** The end was dragged or typed by the user, so it no longer follows the duration. */
	let cEndManual = $state(false);
	let cHostId = $state('');
	let cEventTypeId = $state('');
	/**
	 * The appointment's time zone. Start and end are entered in it, and the booking's
	 * emails and invites to its attendees are written in it.
	 */
	let cTz = $state('');
	/**
	 * Everyone invited, in one list. The first person is the main contact: they get the
	 * confirmation with the reschedule/cancel link. The others are invited as guests.
	 */
	type Person = { name: string; email: string };
	let cPeople = $state<Person[]>([{ name: '', email: '' }]);
	let cConference = $state('');
	let cTitle = $state('');
	let cSync = $state(true);
	let cBusy = $state(false);
	let cError = $state('');
	let cConflicts = $state<Conflict[] | null>(null);

	const cHost = $derived(hosts.find((h) => h.id === cHostId));
	const cEventType = $derived(cHost?.event_types.find((e) => e.id === cEventTypeId));
	const isOnlineMeeting = (t: string) => t === 'google_meet' || t === 'teams';
	const cShowConference = $derived(!!cEventType?.allow_conference_choice && isOnlineMeeting(cEventType.location_type));

	/** The chosen time in the appointment's zone, the viewer's and the host's. */
	const cZoneRows = $derived.by(() => {
		const s = fromInput(cStart, cTz);
		const e = fromInput(cEnd, cTz);
		if (!s || !e || e <= s) return [];
		const parties: { who: string; zone?: string }[] = [];
		if (cMode === 'appointment') parties.push({ who: 'Attendees', zone: cTz });
		parties.push({ who: viewerWho, zone: viewTz });
		if (cHost) parties.push({ who: cHost.self ? 'You' : cHost.name, zone: cHost.timezone });
		return zoneRows(s, e, parties);
	});

	/** The appointment's start, for zone offsets: the offset on that date, not today's (DST). */
	const cAt = $derived(fromInput(cStart, cTz || viewTz) ?? DateTime.now());

	/** Re-express start and end in a new zone, keeping the instants. */
	function setCreateZone(z: string) {
		if (!z || !IANAZone.isValidZone(z) || z === cTz) return;
		const s = fromInput(cStart, cTz);
		const e = fromInput(cEnd, cTz);
		cTz = z;
		if (s) cStart = toInput(s, z);
		if (e) cEnd = toInput(e, z);
	}

	async function addPerson() {
		cPeople = [...cPeople, { name: '', email: '' }];
		await tick();
		document.getElementById(`c-person-name-${cPeople.length - 1}`)?.focus();
	}

	function removePerson(i: number) {
		cPeople = cPeople.filter((_, j) => j !== i);
	}

	// Any change to what is being booked makes a shown conflict list stale.
	$effect(() => {
		void [cMode, cStart, cEnd, cHostId, cEventTypeId];
		cConflicts = null;
	});

	function defaultHostId(): string {
		if (selectedHostIds.length === 1) return selectedHostIds[0];
		const me = hosts.find((h) => h.self && selectedHostIds.includes(h.id));
		if (me) return me.id;
		return selectedHostIds[0] ?? hosts[0]?.id ?? '';
	}

	function applyDefaultEnd() {
		if (cMode !== 'appointment' || cEndManual || !cEventType) return;
		const s = fromInput(cStart, cTz);
		if (s) cEnd = toInput(s.plus({ minutes: cEventType.duration_minutes }), cTz);
	}

	function pickHost(id: string) {
		cHostId = id;
		const h = hosts.find((x) => x.id === id);
		cEventTypeId = h?.event_types[0]?.id ?? '';
		const et = h?.event_types[0];
		cConference = et && isOnlineMeeting(et.location_type) ? et.location_type : '';
		cSync = !!h?.has_calendar;
		applyDefaultEnd();
	}

	function pickEventType(id: string) {
		cEventTypeId = id;
		const et = cHost?.event_types.find((e) => e.id === id);
		cConference = et && isOnlineMeeting(et.location_type) ? et.location_type : '';
		applyDefaultEnd();
	}

	function onStartChange(prev: string) {
		const s = fromInput(cStart, cTz);
		if (!s) return;
		if (cEndManual) {
			// Keep the chosen length when the start moves.
			const ps = fromInput(prev, cTz);
			const pe = fromInput(cEnd, cTz);
			if (ps && pe) cEnd = toInput(s.plus(pe.diff(ps)), cTz);
		} else {
			applyDefaultEnd();
		}
	}

	function setMode(m: Mode) {
		cMode = m;
		cError = '';
		applyDefaultEnd();
	}

	function onSelect(info: DateSelectArg) {
		let start: DateTime;
		let end: DateTime;
		let dragged: boolean;
		if (info.allDay) {
			// Month view: a day was picked, not a time. Start at 09:00 on the first day.
			start = fromFc(info.startStr).set({ hour: 9, minute: 0, second: 0, millisecond: 0 });
			end = start.plus({ minutes: 30 });
			dragged = false;
		} else {
			start = fromFc(info.startStr);
			end = fromFc(info.endStr);
			// A click selects exactly one slot; anything longer was a deliberate drag.
			dragged = end.diff(start, 'minutes').minutes > 30;
		}
		// The dialog starts in the zone the calendar is drawn in, so the times read the same
		// as the range just selected; picking another zone re-expresses them.
		cTz = viewTz;
		cStart = toInput(start, cTz);
		cEnd = toInput(end, cTz);
		cEndManual = dragged;
		cPeople = [{ name: '', email: '' }];
		cTitle = '';
		cError = '';
		cMode = 'appointment';
		pickHost(defaultHostId());
		cConflicts = null;
		createOpen = true;
	}

	function closeCreate() {
		createOpen = false;
		calendar?.unselect();
	}

	const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

	async function submitCreate(force = false) {
		cError = '';
		const s = fromInput(cStart, cTz);
		const e = fromInput(cEnd, cTz);
		if (!s || !e) { cError = 'Enter a valid start and end.'; return; }
		if (e <= s) { cError = 'The end must be after the start.'; return; }
		if (!cHostId) { cError = 'Pick a host.'; return; }

		let path: string;
		let body: Record<string, unknown>;
		if (cMode === 'appointment') {
			if (!cEventTypeId) { cError = 'Pick an event type.'; return; }
			const [main, ...rest] = cPeople.map((p) => ({ name: p.name.trim(), email: p.email.trim() }));
			if (!main?.name) { cError = "Enter the main contact's name."; return; }
			if (!EMAIL_RE.test(main.email)) { cError = "Enter a valid email for the main contact."; return; }
			const guests = rest.filter((p) => p.name || p.email);
			const bad = guests.find((p) => !EMAIL_RE.test(p.email));
			if (bad) { cError = bad.email ? `"${bad.email}" is not a valid email.` : `Enter an email for ${bad.name}.`; return; }
			if (!IANAZone.isValidZone(cTz)) { cError = 'Pick a time zone.'; return; }
			path = '/v1/staff/bookings';
			body = {
				host_id: cHostId,
				event_type_id: cEventTypeId,
				start_at: toUtcIso(s),
				end_at: toUtcIso(e),
				name: main.name,
				email: main.email,
				timezone: cTz
			};
			if (guests.length) body.guests = guests;
			if (cShowConference && cConference) body.conference = cConference;
		} else {
			path = '/v1/unavailable-blocks';
			body = {
				host_id: cHostId,
				start_at: toUtcIso(s),
				end_at: toUtcIso(e),
				sync_to_calendar: !!cHost?.has_calendar && cSync
			};
			if (cTitle.trim()) body.title = cTitle.trim();
		}
		if (force) body.force = true;

		cBusy = true;
		try {
			const res = await api.post<{ sync_error?: string }>(path, body);
			if (res?.sync_error) toast.warning(`Time blocked in Calnode, but ${res.sync_error}`);
			else toast.success(cMode === 'appointment' ? 'Appointment booked' : 'Time blocked');
			closeCreate();
			refetch();
		} catch (err: any) {
			const c = conflictsOf(err);
			if (c) cConflicts = c;
			else cError = err?.message || 'Could not save';
		} finally {
			cBusy = false;
		}
	}

	// ---------------------------------------------------------------------------------
	// Drag / resize
	// ---------------------------------------------------------------------------------

	type MoveInfo = EventDropArg | EventResizeDoneArg;

	let moveOpen = $state(false);
	let moveBusy = $state(false);
	let moveConflicts = $state<Conflict[]>([]);
	let moveTitle = $state('');
	let pendingMove: { info: MoveInfo; send: (force: boolean) => Promise<unknown> } | null = null;

	async function onEventChange(info: MoveInfo) {
		const p = info.event.extendedProps as ItemProps;
		if (p.kind === 'busy' || !info.event.start || !info.event.end) { info.revert(); return; }
		const body = {
			start_at: toUtcIso(fromFc(info.event.startStr)),
			end_at: toUtcIso(fromFc(info.event.endStr))
		};
		const send = (force: boolean) =>
			p.kind === 'booking'
				? api.patch(`/v1/staff/bookings/${p.booking.id}`, force ? { ...body, force: true } : body)
				: api.patch(`/v1/unavailable-blocks/${p.block.id}`, force ? { ...body, force: true } : body);
		try {
			await send(false);
			toast.success(p.kind === 'booking' ? 'Appointment moved' : 'Block updated');
			refetch();
		} catch (err: any) {
			const c = conflictsOf(err);
			if (c) {
				pendingMove = { info, send };
				moveConflicts = c;
				moveTitle = info.event.title;
				moveOpen = true;
			} else {
				info.revert();
				toast.error(err?.message || 'Could not move it');
			}
		}
	}

	function closeMove(revert: boolean) {
		const p = pendingMove;
		pendingMove = null;
		moveOpen = false;
		if (revert) p?.info.revert();
	}

	async function confirmMove() {
		const p = pendingMove;
		if (!p) return;
		moveBusy = true;
		try {
			await p.send(true);
			pendingMove = null;
			moveOpen = false;
			toast.success('Moved');
			refetch();
		} catch (err: any) {
			const c = conflictsOf(err);
			if (c) {
				// e.g. same_start: refused even with force. Leave the dialog up showing why.
				moveConflicts = c;
			} else {
				closeMove(true);
				toast.error(err?.message || 'Could not move it');
			}
		} finally {
			moveBusy = false;
		}
	}

	// ---------------------------------------------------------------------------------
	// Click: booking details / block edit
	// ---------------------------------------------------------------------------------

	let detailsOpen = $state(false);
	let dBooking = $state<StaffBooking | null>(null);

	let blockOpen = $state(false);
	let bBlock = $state<UnavailableBlock | null>(null);
	let bTitle = $state('');
	let bSync = $state(false);
	let bBusy = $state(false);
	let bError = $state('');
	const bHost = $derived(bBlock ? hosts.find((h) => h.id === bBlock!.host_id) : undefined);

	function onEventClick(info: EventClickArg) {
		info.jsEvent.preventDefault();
		const p = info.event.extendedProps as ItemProps;
		if (p.kind === 'booking') {
			dBooking = p.booking;
			resetAddPerson();
			detailsOpen = true;
		} else if (p.kind === 'block') {
			bBlock = p.block;
			bTitle = p.block.title;
			bSync = p.block.sync_to_calendar;
			bError = '';
			blockOpen = true;
		}
	}

	const LOCATION_LABELS: Record<string, string> = {
		zoom: 'Zoom',
		teams: 'Microsoft Teams',
		google_meet: 'Google Meet',
		livekit: 'Calnode Video',
		phone: 'Phone call',
		link: 'Video link',
		in_person: 'In person',
		custom_video: 'Video link'
	};

	const dHostNames = $derived.by(() => {
		if (!dBooking) return '';
		const ids = [dBooking.primary_host_id, ...dBooking.host_ids.filter((id) => id !== dBooking!.primary_host_id)];
		const known = ids.filter((id) => hosts.some((h) => h.id === id)).map(hostName);
		const unknown = ids.length - known.length;
		return [...known, ...(unknown > 0 ? [`${unknown} other${unknown === 1 ? '' : 's'}`] : [])].join(', ');
	});

	// Shared confirm dialog (cancel appointment, delete block).
	let confirmOpen = $state(false);
	let confirmTitle = $state('');
	let confirmDescription = $state('');
	let confirmText = $state('Confirm');
	let confirmCancelText = $state('Cancel');
	let pendingConfirm: (() => void) | null = null;

	function openConfirm(o: { title: string; description: string; confirmText: string; cancelText?: string; action: () => void }) {
		confirmTitle = o.title;
		confirmDescription = o.description;
		confirmText = o.confirmText;
		confirmCancelText = o.cancelText ?? 'Cancel';
		pendingConfirm = o.action;
		confirmOpen = true;
	}

	function requestCancelBooking() {
		const b = dBooking;
		if (!b) return;
		openConfirm({
			title: 'Cancel this appointment?',
			description: `${b.attendee_name || b.attendee_email} will be notified and the time frees up. This can't be undone.`,
			confirmText: 'Cancel appointment',
			cancelText: 'Keep it',
			action: async () => {
				try {
					await api.post(`/v1/staff/bookings/${b.id}/cancel`, {});
					toast.success('Appointment cancelled');
					detailsOpen = false;
					dBooking = null;
					refetch();
				} catch (e: any) {
					toast.error(e?.message || 'Could not cancel the appointment');
				}
			}
		});
	}

	async function saveBlock() {
		const b = bBlock;
		if (!b) return;
		bBusy = true;
		bError = '';
		try {
			const res = await api.patch<{ sync_error?: string }>(`/v1/unavailable-blocks/${b.id}`, {
				title: bTitle.trim(),
				sync_to_calendar: !!bHost?.has_calendar && bSync
			});
			if (res?.sync_error) toast.warning(`Block saved, but ${res.sync_error}`);
			else toast.success('Block updated');
			blockOpen = false;
			refetch();
		} catch (e: any) {
			const c = conflictsOf(e);
			bError = c ? c.map((x) => `${x.host_name}: ${conflictLabel(x)}`).join('; ') : e?.message || 'Could not save';
		} finally {
			bBusy = false;
		}
	}

	function requestDeleteBlock() {
		const b = bBlock;
		if (!b) return;
		openConfirm({
			title: 'Delete this block?',
			description: b.synced
				? 'The time becomes bookable again and the busy event is removed from the host\'s calendar.'
				: 'The time becomes bookable again.',
			confirmText: 'Delete',
			action: async () => {
				try {
					await api.del(`/v1/unavailable-blocks/${b.id}`);
					toast.success('Block deleted');
					blockOpen = false;
					bBlock = null;
					refetch();
				} catch (e: any) {
					toast.error(e?.message || 'Could not delete the block');
				}
			}
		});
	}

	/** The appointment in the calendar's zone, the attendees' zone and each host's. */
	const dZoneRows = $derived.by(() => {
		const b = dBooking;
		if (!b) return [];
		const parties: { who: string; zone?: string }[] = [
			{ who: viewerWho, zone: viewTz },
			{ who: 'Attendees', zone: b.attendee_timezone }
		];
		for (const id of b.host_ids) {
			const h = hosts.find((x) => x.id === id);
			if (h) parties.push({ who: h.self ? 'You' : h.name, zone: h.timezone });
		}
		return zoneRows(DateTime.fromISO(b.start_at), DateTime.fromISO(b.end_at), parties);
	});

	/** Main contact first, then the guests, as one list. */
	const dPeople = $derived.by(() => {
		const b = dBooking;
		if (!b) return [];
		const guests = b.guest_list ?? (b.guests ?? []).map((email) => ({ name: '', email }));
		return [{ name: b.attendee_name, email: b.attendee_email, main: true }, ...guests.map((g) => ({ ...g, main: false }))];
	});

	const dCanAdd = $derived(
		!!dBooking && dBooking.can_manage && dBooking.status === 'confirmed' && DateTime.fromISO(dBooking.end_at) > DateTime.now()
	);

	// Add an attendee to an existing appointment: they are invited straight away.
	let aOpen = $state(false);
	let aName = $state('');
	let aEmail = $state('');
	let aBusy = $state(false);
	let aError = $state('');

	function resetAddPerson() {
		aOpen = false;
		aName = '';
		aEmail = '';
		aError = '';
	}

	async function openAddPerson() {
		resetAddPerson();
		aOpen = true;
		await tick();
		document.getElementById('a-name')?.focus();
	}

	async function addAttendee() {
		const b = dBooking;
		if (!b) return;
		aError = '';
		const email = aEmail.trim();
		if (!EMAIL_RE.test(email)) { aError = 'Enter a valid email.'; return; }
		aBusy = true;
		try {
			const res = await api.post<{ guests?: { name: string; email: string }[]; warning?: string; invited_via?: string }>(
				`/v1/bookings/${b.id}/guests`, { name: aName.trim(), email }
			);
			const list = res?.guests ?? [...(b.guest_list ?? []), { name: aName.trim(), email }];
			dBooking = { ...b, guest_list: list, guests: list.map((g) => g.email) };
			if (res?.warning) toast.warning(res.warning);
			else toast.success(res?.invited_via === 'calendar' ? 'Attendee added and invited from the calendar' : 'Attendee added and emailed an invitation');
			resetAddPerson();
			refetch();
		} catch (e: any) {
			aError = e?.message || 'Could not add the attendee';
		} finally {
			aBusy = false;
		}
	}

	const isUrl = (s: string) => /^https?:\/\//i.test(s);
</script>

<svelte:head><title>Schedule — Calnode</title></svelte:head>

<div class="mb-6 flex flex-wrap items-start justify-between gap-4">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Schedule</h1>
		<p class="mt-1 text-sm text-muted-foreground">
			Drag across a time to book an appointment or block it off. Drag an appointment to move it.
		</p>
		<p class="mt-1 text-sm">
			All times on this calendar are in <span class="font-medium">{zoneLabel(viewTz)}</span>{#if viewTz !== profileTz}<span class="text-muted-foreground">, not your profile zone ({zoneLabel(profileTz)})</span>{/if}.
		</p>
	</div>
	<div class="flex w-full flex-wrap items-center gap-2 sm:w-auto sm:shrink-0">
		<div class="flex w-full items-center gap-1.5 sm:w-auto" title="Time zone the calendar is shown in">
			<GlobeIcon class="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
			<span class="sr-only">Time zone</span>
			<Combobox
				class="min-w-0 flex-1 sm:w-64 sm:flex-none"
				items={ZONE_ITEMS}
				bind:value={() => viewTz, setViewTz}
				searchPlaceholder="Search by city, region or offset…"
			/>
		</div>
		{#if viewTz !== profileTz}
			<Button variant="ghost" size="sm" onclick={() => setViewTz(profileTz)}>Use my zone</Button>
		{/if}
		{#if multiHost}
			<Popover.Root>
				<Popover.Trigger class={buttonVariants({ variant: 'outline', size: 'sm' })} aria-label="Choose hosts">
					<span class="flex -space-x-1">
						{#each selectedHostIds.slice(0, 4) as id (id)}
							<span class="h-2.5 w-2.5 rounded-full ring-2 ring-background" style="background:{hostColor(id)}"></span>
						{/each}
					</span>
					{selectionLabel}
				</Popover.Trigger>
				<Popover.Content align="end" class="w-64 p-2">
					<div class="mb-1 flex items-center justify-between px-2 py-1">
						<p class="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Hosts</p>
						<div class="flex gap-2 text-xs">
							<button type="button" class="text-muted-foreground hover:text-foreground" onclick={() => setSelection(hosts.map((h) => h.id))}>All</button>
							<button type="button" class="text-muted-foreground hover:text-foreground" onclick={() => setSelection([])}>None</button>
						</div>
					</div>
					<div class="max-h-72 overflow-y-auto">
						{#each hosts as h (h.id)}
							<label class="flex cursor-pointer items-center gap-2.5 rounded-md px-2 py-1.5 text-sm hover:bg-accent">
								<Checkbox
									checked={selectedHostIds.includes(h.id)}
									onCheckedChange={(v) => toggleHost(h.id, v === true)}
									aria-label="Show {h.name}"
								/>
								<span class="h-2.5 w-2.5 shrink-0 rounded-full" style="background:{hostColor(h.id)}"></span>
								<span class="min-w-0 flex-1 truncate">{h.name}</span>
								{#if h.self}<span class="text-xs text-muted-foreground">you</span>{/if}
							</label>
						{/each}
					</div>
				</Popover.Content>
			</Popover.Root>
		{/if}
		<Button variant="outline" size="sm" onclick={refetch} aria-label="Refresh schedule">
			<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M3 21v-5h5"/></svg>
			<span class="hidden sm:inline">Refresh</span>
		</Button>
	</div>
</div>

{#if hostsError}
	<p class="mb-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{hostsError}</p>
{/if}
{#if fetchError}
	<p class="mb-4 rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{fetchError}</p>
{/if}
{#if busyErrors.length > 0 && !busyErrorsDismissed}
	<div class="mb-4 flex items-start gap-3 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900 dark:border-amber-900/50 dark:bg-amber-950/40 dark:text-amber-200">
		<div class="min-w-0 flex-1">
			<p class="font-medium">Some calendars couldn't be read, so their busy time isn't shown.</p>
			<ul class="mt-0.5 text-xs">
				{#each busyErrors as be}
					<li>{hostName(be.host_id)}: {be.error}</li>
				{/each}
			</ul>
		</div>
		<button type="button" class="shrink-0 text-xs opacity-70 hover:opacity-100" onclick={() => (busyErrorsDismissed = true)}>Dismiss</button>
	</div>
{/if}

{#if multiHost && selectedHostIds.length > 1}
	<div class="mb-3 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
		{#each selectedHostIds as id (id)}
			<span class="inline-flex items-center gap-1.5">
				<span class="h-2.5 w-2.5 rounded-full" style="background:{hostColor(id)}"></span>{hostName(id)}
			</span>
		{/each}
	</div>
{/if}

{#if loadingHosts}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{/if}
<div class="schedule-cal" class:hidden={loadingHosts || !!hostsError} style="--cn-tz-corner: '{cornerLabel(viewTz)}'">
	<div bind:this={calendarEl} class="h-full"></div>
</div>

<!-- Create: appointment or unavailable block -->
<Dialog.Root bind:open={createOpen} onOpenChange={(o) => { if (!o) calendar?.unselect(); }}>
	<Dialog.Content class="max-h-[90vh] max-w-lg overflow-y-auto">
		<Dialog.Header>
			<Dialog.Title>{cMode === 'appointment' ? 'New appointment' : 'Block time'}</Dialog.Title>
			<Dialog.Description>Start and end are in {zoneLabel(cTz || viewTz, cAt)}.</Dialog.Description>
		</Dialog.Header>

		<div class="inline-flex w-fit rounded-md border p-0.5 text-sm" role="tablist">
			<button type="button" role="tab" aria-selected={cMode === 'appointment'} class="rounded px-3 py-1 transition-colors {cMode === 'appointment' ? 'bg-secondary font-medium text-secondary-foreground' : 'text-muted-foreground hover:text-foreground'}" onclick={() => setMode('appointment')}>Appointment</button>
			<button type="button" role="tab" aria-selected={cMode === 'unavailable'} class="rounded px-3 py-1 transition-colors {cMode === 'unavailable' ? 'bg-secondary font-medium text-secondary-foreground' : 'text-muted-foreground hover:text-foreground'}" onclick={() => setMode('unavailable')}>Unavailable</button>
		</div>

		<div class="grid gap-4">
			<div class="space-y-1.5">
				<Label>Time zone</Label>
				<Combobox
					items={ZONE_ITEMS}
					bind:value={() => cTz, setCreateZone}
					searchPlaceholder="Search by city, region or offset…"
				/>
				{#if cMode === 'appointment'}
					<p class="text-xs text-muted-foreground">The attendees' time zone: their emails and invites show times in it.</p>
				{/if}
			</div>
			<div class="grid gap-3 sm:grid-cols-2">
				<div class="space-y-1.5">
					<Label for="c-start">Start <span class="font-normal text-muted-foreground">({zoneOffset(cTz || viewTz, cAt)})</span></Label>
					<Input id="c-start" type="datetime-local" value={cStart}
						onchange={(e) => { const prev = cStart; cStart = e.currentTarget.value; onStartChange(prev); }} />
				</div>
				<div class="space-y-1.5">
					<Label for="c-end">End <span class="font-normal text-muted-foreground">({zoneOffset(cTz || viewTz, cAt)})</span></Label>
					<Input id="c-end" type="datetime-local" value={cEnd}
						onchange={(e) => { cEnd = e.currentTarget.value; cEndManual = true; }} />
				</div>
			</div>
			{#if cZoneRows.length > 0}
				{@render zoneTable(cZoneRows)}
			{/if}

			{#if hosts.length > 1}
				<div class="space-y-1.5">
					<Label>Host</Label>
					<Select.Root type="single" value={cHostId} onValueChange={(v) => v && pickHost(v)}>
						<Select.Trigger class="w-full" aria-label="Host">
							<span class="flex items-center gap-2">
								{#if cHostId}<span class="h-2.5 w-2.5 rounded-full" style="background:{hostColor(cHostId)}"></span>{/if}
								{cHost?.name ?? 'Pick a host'}
							</span>
						</Select.Trigger>
						<Select.Content>
							{#each hosts as h (h.id)}
								<Select.Item value={h.id} label={h.name}>{h.name}{h.self ? ' (you)' : ''}</Select.Item>
							{/each}
						</Select.Content>
					</Select.Root>
				</div>
			{/if}

			{#if cMode === 'appointment'}
				{#if cHost && cHost.event_types.length === 0}
					<p class="rounded-md bg-muted px-3 py-2 text-sm text-muted-foreground">{cHost.name} has no active event types to book.</p>
				{:else}
					<div class="space-y-1.5">
						<Label>Event type</Label>
						<Select.Root type="single" value={cEventTypeId} onValueChange={(v) => v && pickEventType(v)}>
							<Select.Trigger class="w-full" aria-label="Event type">
								{cEventType ? `${cEventType.name} · ${cEventType.duration_minutes} min` : 'Pick an event type'}
							</Select.Trigger>
							<Select.Content>
								{#each cHost?.event_types ?? [] as et (et.id)}
									<Select.Item value={et.id} label={et.name}>{et.name} · {et.duration_minutes} min</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					</div>
				{/if}

				<fieldset class="space-y-2">
					<legend class="text-sm font-medium">Attendees</legend>
					{#each cPeople as person, i (i)}
						<div class="flex items-start gap-2">
							<div class="grid flex-1 gap-2 sm:grid-cols-2">
								<Input id="c-person-name-{i}" bind:value={person.name} autocomplete="off"
									placeholder={i === 0 ? 'Name' : 'Name (optional)'} aria-label="Attendee {i + 1} name" />
								<Input id="c-person-email-{i}" type="email" bind:value={person.email} autocomplete="off"
									placeholder="Email" aria-label="Attendee {i + 1} email" />
							</div>
							{#if i === 0}
								<span class="mt-1.5 w-8 shrink-0 text-center text-[0.625rem] leading-tight text-muted-foreground" title="Gets the confirmation with the reschedule and cancel link">main</span>
							{:else}
								<Button variant="ghost" size="icon-sm" class="mt-0.5 shrink-0" onclick={() => removePerson(i)} aria-label="Remove attendee {i + 1}">
									<XIcon class="size-4" />
								</Button>
							{/if}
						</div>
					{/each}
					<div class="flex flex-wrap items-center justify-between gap-2">
						<Button variant="outline" size="sm" onclick={addPerson} disabled={cPeople.length > 50}>
							<PlusIcon class="size-4" /> Add attendee
						</Button>
						<p class="text-xs text-muted-foreground">The first person gets the confirmation with the reschedule link; everyone is invited.</p>
					</div>
				</fieldset>

				{#if cShowConference}
					<div class="space-y-1.5">
						<Label>Conference</Label>
						<Select.Root type="single" value={cConference} onValueChange={(v) => v && (cConference = v)}>
							<Select.Trigger class="w-full" aria-label="Conference">
								{cConference === 'teams' ? 'Microsoft Teams' : 'Google Meet'}
							</Select.Trigger>
							<Select.Content>
								<Select.Item value="google_meet" label="Google Meet">Google Meet</Select.Item>
								<Select.Item value="teams" label="Microsoft Teams">Microsoft Teams</Select.Item>
							</Select.Content>
						</Select.Root>
					</div>
				{/if}
			{:else}
				<div class="space-y-1.5">
					<Label for="c-title">Title <span class="font-normal text-muted-foreground">(optional)</span></Label>
					<Input id="c-title" bind:value={cTitle} placeholder="Unavailable" />
				</div>
				<div class="flex items-start justify-between gap-4">
					<div>
						<Label for="c-sync">Also block on the host's calendar</Label>
						<p class="mt-0.5 text-xs text-muted-foreground">
							{cHost?.has_calendar
								? 'Adds a busy event with no attendees to their calendar.'
								: `${cHost?.name ?? 'This host'} has no connected calendar.`}
						</p>
					</div>
					<Switch id="c-sync" checked={!!cHost?.has_calendar && cSync} onCheckedChange={(v) => (cSync = v)} disabled={!cHost?.has_calendar} />
				</div>
			{/if}
		</div>

		{#if cConflicts && cConflicts.length > 0}
			{@const blocking = isBlocking(cConflicts)}
			<div class="rounded-md border px-3 py-2 text-sm {blocking
				? 'border-destructive/30 bg-destructive/10 text-destructive'
				: 'border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-900/50 dark:bg-amber-950/40 dark:text-amber-200'}">
				<p class="font-medium">{blocking ? "This time can't be used:" : 'Heads up — this time has conflicts:'}</p>
				<ul class="mt-1 list-disc space-y-0.5 pl-5">
					{#each cConflicts as c}
						<li>
							<span class:font-semibold={BLOCKING_KINDS.has(c.kind)}>{c.host_name}: {conflictLabel(c)}</span>
							{#if c.detail}<span class="opacity-75"> — {c.detail}</span>{/if}
						</li>
					{/each}
				</ul>
			</div>
		{/if}

		{#if cError}<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{cError}</p>{/if}

		<Dialog.Footer class="gap-2">
			<Button variant="outline" onclick={closeCreate} disabled={cBusy}>Cancel</Button>
			{#if cConflicts && cConflicts.length > 0 && !isBlocking(cConflicts)}
				<Button onclick={() => submitCreate(true)} disabled={cBusy}>{cBusy ? 'Saving…' : 'Save anyway'}</Button>
			{:else}
				<Button onclick={() => submitCreate(false)}
					disabled={cBusy || isBlocking(cConflicts) || (cMode === 'appointment' && !cEventTypeId)}>
					{cBusy ? 'Saving…' : cMode === 'appointment' ? 'Book appointment' : 'Block time'}
				</Button>
			{/if}
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<!-- Move / resize conflict confirmation -->
<Dialog.Root bind:open={moveOpen} onOpenChange={(o) => { if (!o && pendingMove) closeMove(true); }}>
	<Dialog.Content class="max-w-md">
		{@const blocking = isBlocking(moveConflicts)}
		<Dialog.Header>
			<Dialog.Title>{blocking ? "Can't move here" : 'Move anyway?'}</Dialog.Title>
			<Dialog.Description>{moveTitle}</Dialog.Description>
		</Dialog.Header>
		<ul class="list-disc space-y-0.5 pl-5 text-sm">
			{#each moveConflicts as c}
				<li class={BLOCKING_KINDS.has(c.kind) ? 'font-medium text-destructive' : ''}>
					{c.host_name}: {conflictLabel(c)}{#if c.detail}<span class="text-muted-foreground"> — {c.detail}</span>{/if}
				</li>
			{/each}
		</ul>
		<Dialog.Footer class="gap-2">
			<Button variant="outline" onclick={() => closeMove(true)} disabled={moveBusy}>{blocking ? 'OK' : 'Cancel'}</Button>
			{#if !blocking}
				<Button onclick={confirmMove} disabled={moveBusy}>{moveBusy ? 'Moving…' : 'Move anyway'}</Button>
			{/if}
		</Dialog.Footer>
	</Dialog.Content>
</Dialog.Root>

<!-- Booking details -->
<Dialog.Root bind:open={detailsOpen}>
	<Dialog.Content class="max-w-md">
		{#if dBooking}
			{@const b = dBooking}
			<Dialog.Header>
				<Dialog.Title>{b.event_type_name}</Dialog.Title>
				<Dialog.Description>{fmtRange(b.start_at, b.end_at)} · {zoneLabel(viewTz, DateTime.fromISO(b.start_at))}</Dialog.Description>
			</Dialog.Header>
			{#if dZoneRows.length > 1}
				{@render zoneTable(dZoneRows)}
			{/if}
			<dl class="space-y-2 text-sm">
				<div class="flex gap-4">
					<dt class="w-24 shrink-0 font-medium">Attendees</dt>
					<dd class="min-w-0 flex-1 text-muted-foreground">
						<ul class="space-y-1">
							{#each dPeople as person (person.email)}
								<li class="min-w-0">
									{#if person.name}<span class="text-foreground">{person.name}</span>{/if}
									<a href="mailto:{person.email}" class="break-all text-primary hover:underline">{person.email}</a>
									{#if person.main}<span class="ml-1 rounded bg-muted px-1 py-0.5 text-[0.625rem] uppercase tracking-wide" title="Gets the confirmation with the reschedule and cancel link">main</span>{/if}
								</li>
							{/each}
						</ul>
						{#if dCanAdd}
							{#if aOpen}
								<form class="mt-2 grid gap-2" onsubmit={(e) => { e.preventDefault(); addAttendee(); }}>
									<Input id="a-name" bind:value={aName} placeholder="Name (optional)" autocomplete="off" aria-label="New attendee name" />
									<Input id="a-email" type="email" bind:value={aEmail} placeholder="Email" autocomplete="off" aria-label="New attendee email" />
									{#if aError}<p class="text-xs text-destructive">{aError}</p>{/if}
									<div class="flex gap-2">
										<Button type="submit" size="sm" disabled={aBusy}>{aBusy ? 'Adding…' : 'Add and invite'}</Button>
										<Button type="button" variant="ghost" size="sm" onclick={resetAddPerson} disabled={aBusy}>Cancel</Button>
									</div>
								</form>
							{:else}
								<Button variant="outline" size="sm" class="mt-2" onclick={openAddPerson}>
									<PlusIcon class="size-4" /> Add attendee
								</Button>
							{/if}
						{/if}
					</dd>
				</div>
				<div class="flex gap-4">
					<dt class="w-24 shrink-0 font-medium">{b.host_ids.length > 1 ? 'Hosts' : 'Host'}</dt>
					<dd class="min-w-0 text-muted-foreground">{dHostNames}</dd>
				</div>
				{#if b.location_value || b.location_type}
					<div class="flex gap-4">
						<dt class="w-24 shrink-0 font-medium">Location</dt>
						<dd class="min-w-0 break-all text-muted-foreground">
							{#if b.location_value && isUrl(b.location_value)}
								<a href={b.location_value} target="_blank" rel="noopener noreferrer" class="text-primary underline">{b.location_value}</a>
							{:else if b.location_value}
								{b.location_value}
							{:else}
								{LOCATION_LABELS[b.location_type] ?? b.location_type}
							{/if}
						</dd>
					</div>
				{/if}
			</dl>
			<Dialog.Footer class="gap-2">
				{#if b.can_manage}
					<Button variant="ghost" class="text-destructive hover:text-destructive" onclick={requestCancelBooking}>Cancel appointment</Button>
				{/if}
				<Button variant="outline" onclick={() => (detailsOpen = false)}>Close</Button>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>

<!-- Unavailable block edit -->
<Dialog.Root bind:open={blockOpen}>
	<Dialog.Content class="max-w-md">
		{#if bBlock}
			<Dialog.Header>
				<Dialog.Title>Unavailable block</Dialog.Title>
				<Dialog.Description>
					{fmtRange(bBlock.start_at, bBlock.end_at)} · {zoneLabel(viewTz, DateTime.fromISO(bBlock.start_at))}{#if multiHost} · {hostName(bBlock.host_id)}{/if}
				</Dialog.Description>
			</Dialog.Header>
			<div class="grid gap-4">
				<div class="space-y-1.5">
					<Label for="b-title">Title</Label>
					<Input id="b-title" bind:value={bTitle} placeholder="Unavailable" onkeydown={(e) => e.key === 'Enter' && saveBlock()} />
				</div>
				<div class="flex items-start justify-between gap-4">
					<div>
						<Label for="b-sync">Also block on the host's calendar</Label>
						<p class="mt-0.5 text-xs text-muted-foreground">
							{#if !bHost?.has_calendar}
								{bHost?.name ?? 'This host'} has no connected calendar.
							{:else if bBlock.synced}
								On their calendar now. Turning this off removes it.
							{:else}
								Adds a busy event with no attendees to their calendar.
							{/if}
						</p>
					</div>
					<Switch id="b-sync" checked={!!bHost?.has_calendar && bSync} onCheckedChange={(v) => (bSync = v)} disabled={!bHost?.has_calendar} />
				</div>
				<p class="text-xs text-muted-foreground">Drag the block on the calendar to change its time.</p>
			</div>
			{#if bError}<p class="rounded-md bg-destructive/10 px-3 py-2 text-sm text-destructive">{bError}</p>{/if}
			<Dialog.Footer class="gap-2 sm:justify-between">
				<Button variant="ghost" class="text-destructive hover:text-destructive" onclick={requestDeleteBlock} disabled={bBusy}>Delete</Button>
				<div class="flex gap-2">
					<Button variant="outline" onclick={() => (blockOpen = false)} disabled={bBusy}>Cancel</Button>
					<Button onclick={saveBlock} disabled={bBusy}>{bBusy ? 'Saving…' : 'Save'}</Button>
				</div>
			</Dialog.Footer>
		{/if}
	</Dialog.Content>
</Dialog.Root>

{#snippet zoneTable(rows: ZoneRow[])}
	<dl class="grid gap-1.5 rounded-md border bg-muted/40 px-3 py-2 text-xs">
		{#each rows as r (r.zone)}
			<div class="flex flex-wrap items-baseline justify-between gap-x-3">
				<dt class="text-muted-foreground"><span class="font-medium text-foreground">{r.who}</span> · {r.label}</dt>
				<dd class="font-medium tabular-nums">{r.when}</dd>
			</div>
		{/each}
	</dl>
{/snippet}

<ConfirmDialog
	bind:open={confirmOpen}
	title={confirmTitle}
	description={confirmDescription}
	confirmText={confirmText}
	cancelText={confirmCancelText}
	destructive
	onConfirm={() => pendingConfirm?.()}
/>

<style>
	.schedule-cal {
		height: max(560px, calc(100vh - 15rem));

		/* FullCalendar theme variables mapped onto the app's tokens (light + .dark). */
		--fc-small-font-size: 0.75rem;
		--fc-page-bg-color: var(--background);
		--fc-neutral-bg-color: var(--muted);
		--fc-neutral-text-color: var(--muted-foreground);
		--fc-border-color: var(--border);
		--fc-button-text-color: var(--foreground);
		--fc-button-bg-color: var(--background);
		--fc-button-border-color: var(--border);
		--fc-button-hover-bg-color: var(--accent);
		--fc-button-hover-border-color: var(--border);
		--fc-button-active-bg-color: var(--secondary);
		--fc-button-active-border-color: var(--border);
		--fc-event-bg-color: var(--primary);
		--fc-event-border-color: var(--primary);
		--fc-event-text-color: var(--primary-foreground);
		--fc-event-selected-overlay-color: color-mix(in oklch, var(--foreground) 20%, transparent);
		--fc-more-link-bg-color: var(--muted);
		--fc-more-link-text-color: var(--foreground);
		--fc-today-bg-color: color-mix(in oklch, var(--accent) 60%, transparent);
		--fc-now-indicator-color: var(--destructive);
		--fc-highlight-color: color-mix(in oklch, var(--ring) 25%, transparent);
		--fc-non-business-color: color-mix(in oklch, var(--muted) 50%, transparent);
		--fc-bg-event-color: var(--muted-foreground);
		--fc-bg-event-opacity: 0.18;
	}

	.schedule-cal :global(.fc) {
		font-size: 0.8125rem;
		color: var(--foreground);
	}
	.schedule-cal :global(.fc .fc-toolbar) {
		flex-wrap: wrap;
		gap: 0.5rem;
	}
	.schedule-cal :global(.fc .fc-toolbar.fc-header-toolbar) {
		margin-bottom: 0.75rem;
	}
	.schedule-cal :global(.fc .fc-toolbar-title) {
		font-size: 1rem;
		font-weight: 600;
	}
	.schedule-cal :global(.fc .fc-button) {
		border-radius: calc(var(--radius) - 2px);
		padding: 0.25rem 0.625rem;
		font-size: 0.8125rem;
		font-weight: 500;
		box-shadow: none;
		text-transform: none;
	}
	.schedule-cal :global(.fc .fc-button:focus),
	.schedule-cal :global(.fc .fc-button-primary:not(:disabled):active:focus),
	.schedule-cal :global(.fc .fc-button-primary:not(:disabled).fc-button-active:focus) {
		box-shadow: 0 0 0 2px color-mix(in oklch, var(--ring) 50%, transparent);
	}
	.schedule-cal :global(.fc .fc-button-primary:disabled) {
		color: var(--muted-foreground);
		background-color: var(--background);
		border-color: var(--border);
	}
	/* The zone's UTC offset in the corner above the hour labels, as in Google Calendar. */
	.schedule-cal :global(.fc .fc-scrollgrid-section-header .fc-timegrid-axis-frame) {
		align-items: flex-end;
		justify-content: center;
	}
	.schedule-cal :global(.fc .fc-scrollgrid-section-header .fc-timegrid-axis-frame)::after {
		content: var(--cn-tz-corner);
		white-space: pre;
		padding: 0 2px 2px;
		font-size: 0.625rem;
		line-height: 1.1;
		text-align: center;
		color: var(--muted-foreground);
	}
	.schedule-cal :global(.fc .fc-col-header-cell-cushion),
	.schedule-cal :global(.fc .fc-daygrid-day-number) {
		color: var(--foreground);
		font-weight: 500;
	}
	.schedule-cal :global(.fc .fc-timegrid-slot-label-cushion),
	.schedule-cal :global(.fc .fc-timegrid-axis-cushion) {
		color: var(--muted-foreground);
	}
	.schedule-cal :global(.fc .fc-timegrid-slot) {
		height: 2.25em;
	}
	.schedule-cal :global(.fc .fc-event) {
		border-radius: calc(var(--radius) - 4px);
	}
	.schedule-cal :global(.fc .cn-booking.cn-manageable) {
		cursor: pointer;
	}
	.schedule-cal :global(.fc .cn-booking.cn-readonly) {
		cursor: pointer;
		opacity: 0.75;
	}

	/* Unavailable blocks: hatched grey, host colour on the edge. */
	.schedule-cal :global(.fc .fc-event.cn-block) {
		cursor: pointer;
		border-width: 1px 1px 1px 3px;
		background-color: var(--muted);
		background-image: repeating-linear-gradient(
			-45deg,
			color-mix(in oklch, var(--muted-foreground) 22%, transparent) 0 6px,
			transparent 6px 12px
		);
	}
	.schedule-cal :global(.fc .fc-event.cn-block .fc-event-main),
	.schedule-cal :global(.fc .fc-event.cn-block .fc-event-title),
	.schedule-cal :global(.fc .fc-event.cn-block .fc-event-time) {
		color: var(--foreground);
	}

	/* External calendar busy: light, non-interactive background. */
	.schedule-cal :global(.fc .fc-bg-event.cn-busy) {
		cursor: default;
	}
	.schedule-cal :global(.fc .fc-bg-event.cn-busy .fc-event-title) {
		color: var(--foreground);
		font-size: 0.6875rem;
		opacity: 0.9;
	}

	@media (max-width: 640px) {
		.schedule-cal :global(.fc .fc-toolbar.fc-header-toolbar) {
			flex-direction: column;
			align-items: stretch;
		}
		.schedule-cal :global(.fc .fc-toolbar-chunk) {
			display: flex;
			justify-content: center;
		}
	}
</style>
