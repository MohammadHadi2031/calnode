<script lang="ts">
	import { parseDate, today, getLocalTimeZone, type DateValue } from '@internationalized/date';
	import { Calendar } from '$lib/components/ui/calendar';
	import * as Popover from '$lib/components/ui/popover';
	import { buttonVariants } from '$lib/components/ui/button';
	import { cn } from '$lib/utils.js';

	let {
		value = $bindable(''),
		placeholder = 'Pick a date',
		minToday = false,
		timeZone = undefined,
		class: className = '',
	}: {
		value?: string;
		placeholder?: string;
		minToday?: boolean;
		/** IANA zone whose "today" is marked and used for minToday. Default: the browser's. */
		timeZone?: string;
		class?: string;
	} = $props();

	let open = $state(false);

	const tz = $derived(timeZone || getLocalTimeZone());

	let calValue = $state<DateValue | undefined>(undefined);
	$effect(() => {
		calValue = value ? parseDate(value) : undefined;
	});

	const minValue = $derived(minToday ? today(tz) : undefined);

	function fmt(iso: string) {
		try {
			// A calendar date, not an instant: format it as such so no zone can shift the day.
			return parseDate(iso).toDate('UTC').toLocaleDateString([], {
				day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC'
			});
		} catch { return iso; }
	}

	function onCalChange(v: DateValue | undefined) {
		value = v ? v.toString() : '';
		open = false;
	}
</script>

<Popover.Root bind:open>
	<Popover.Trigger
		class={cn(
			buttonVariants({ variant: 'outline' }),
			'w-[200px] justify-start text-left font-normal',
			!value && 'text-muted-foreground',
			className
		)}
	>
		<svg xmlns="http://www.w3.org/2000/svg" class="mr-2 size-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="18" rx="2" ry="2"/><line x1="16" y1="2" x2="16" y2="6"/><line x1="8" y1="2" x2="8" y2="6"/><line x1="3" y1="10" x2="21" y2="10"/></svg>
		{value ? fmt(value) : placeholder}
	</Popover.Trigger>
	<Popover.Content class="w-auto p-0" align="start">
		<Calendar
			bind:value={calValue}
			onValueChange={onCalChange}
			{minValue}
			timeZone={tz}
		/>
	</Popover.Content>
</Popover.Root>
