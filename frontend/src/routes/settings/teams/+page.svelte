<script lang="ts">
	import { onMount } from 'svelte';
	import { api, type TeamsSettings, type TeamsCheck } from '$lib/api';
	import { currentUser } from '$lib/stores';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Label } from '$lib/components/ui/label';
	import { ConfirmDialog } from '$lib/components/ui/confirm-dialog';
	import { toast } from 'svelte-sonner';
	import { saveOnCmdS } from '$lib/save-shortcut';
	import { createAsyncFlag } from '$lib/async-action.svelte';

	// The workspace Teams account: one Microsoft 365 mailbox that organizes the Teams
	// meeting for every host without a Microsoft work account of their own. The server
	// reaches it with the app's own credentials, so nobody signs in as it here.

	const loadingFlag = createAsyncFlag(true);
	const savingFlag = createAsyncFlag();
	const testingFlag = createAsyncFlag();

	let settings = $state<TeamsSettings | null>(null);
	let mailbox = $state('');
	let check = $state<TeamsCheck | null>(null);
	let confirmOff = $state(false);

	const dirty = $derived(mailbox.trim().toLowerCase() !== (settings?.mailbox ?? ''));
	const configured = $derived(!!settings?.mailbox);

	onMount(() => loadingFlag.run(async () => {
		settings = await api.get<TeamsSettings>('/v1/settings/teams');
		mailbox = settings.mailbox;
	}, 'Could not load Microsoft Teams settings'));

	async function save(value = mailbox) {
		await savingFlag.run(async () => {
			settings = await api.patch<TeamsSettings>('/v1/settings/teams', { mailbox: value.trim() });
			mailbox = settings.mailbox;
			check = null;
			toast.success(settings.mailbox
				? 'Saved. New Teams bookings use this account.'
				: 'Turned off. Hosts need their own Microsoft work account or a fixed Teams link.');
		}, 'Could not save Microsoft Teams settings');
	}

	async function test() {
		await testingFlag.run(async () => {
			check = await api.post<TeamsCheck>('/v1/settings/teams/test', { mailbox: mailbox.trim() });
		}, 'Could not run the test');
	}
</script>

<svelte:window onkeydown={saveOnCmdS(() => save(), () => !savingFlag.active && dirty)} />

{#if !$currentUser?.is_admin}
	<p class="text-sm text-muted-foreground">Admin access required.</p>
{:else if loadingFlag.active}
	<p class="py-8 text-sm text-muted-foreground">Loading…</p>
{:else if settings}
	<div class="max-w-lg space-y-4">
		<div class="rounded-lg border bg-card p-6">
			<div class="mb-4 flex items-start justify-between gap-2">
				<div>
					<h2 class="text-sm font-semibold">Workspace Teams account</h2>
					<p class="mt-0.5 text-xs text-muted-foreground">
						One Microsoft 365 account that creates the Teams meeting for every host, so hosts
						don't need a Microsoft license or have to connect anything. It invites the host and
						the booker, and is never checked for conflicts. A host who connects their own
						Microsoft work account still uses that one.
					</p>
				</div>
				<span class="flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium {configured ? 'bg-green-50 text-green-700' : 'bg-amber-50 text-amber-700'}">
					<span class="h-1.5 w-1.5 rounded-full {configured ? 'bg-green-500' : 'bg-amber-400'}"></span>
					{configured ? 'On' : 'Off'}
				</span>
			</div>

			{#if !settings.available}
				<p class="rounded-md bg-amber-50 px-3 py-2.5 text-xs text-amber-800 ring-1 ring-inset ring-amber-200">
					{settings.reason}
				</p>
			{:else}
				<div class="space-y-1.5">
					<Label for="teams-mailbox">Account email</Label>
					<Input id="teams-mailbox" type="email" autocomplete="off"
						placeholder="appointments@yourcompany.onmicrosoft.com" bind:value={mailbox} />
					<p class="text-xs text-muted-foreground">
						A licensed user in your organization with Exchange Online and Microsoft Teams.
					</p>
				</div>

				{#if check}
					<div class="mt-4 rounded-md px-3 py-2.5 text-xs ring-1 ring-inset {check.ok ? 'bg-green-50 text-green-800 ring-green-200' : 'bg-amber-50 text-amber-800 ring-amber-200'}">
						{#if check.ok}
							<span class="font-medium">Ready.</span> Calnode can reach this mailbox, and Microsoft allows it to create Teams meetings.
						{:else}
							<span class="font-medium">{check.reachable ? 'Not ready for Teams yet.' : 'Not reachable.'}</span> {check.message}
						{/if}
					</div>
				{/if}

				<div class="mt-5 flex flex-wrap gap-2">
					<Button onclick={() => save()} disabled={savingFlag.active || !dirty || !mailbox.trim()}>
						{savingFlag.active ? 'Saving…' : 'Save'}
					</Button>
					<Button variant="outline" onclick={test} disabled={testingFlag.active || !mailbox.trim()}>
						{testingFlag.active ? 'Testing…' : 'Test'}
					</Button>
					{#if configured}
						<Button variant="destructive" class="ml-auto"
							onclick={() => (confirmOff = true)} disabled={savingFlag.active}>
							Turn off
						</Button>
					{/if}
				</div>
			{/if}
		</div>

		{#if settings.available}
			<div class="rounded-lg border bg-card p-6">
				<h2 class="mb-4 text-sm font-semibold">Microsoft-side setup</h2>
				<ol class="space-y-4 text-sm">
					<li class="flex gap-3">
						<span class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-muted-foreground">1</span>
						<div>
							In <a href="https://entra.microsoft.com" target="_blank" rel="noopener noreferrer" class="font-medium text-primary underline">Microsoft Entra</a>
							→ App registrations → this server's app → <span class="font-medium">API permissions</span>, add
							<span class="font-medium">Microsoft Graph → Application permissions → Calendars.ReadWrite</span>,
							then <span class="font-medium">Grant admin consent</span>.
						</div>
					</li>
					<li class="flex gap-3">
						<span class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-muted-foreground">2</span>
						<div>
							Recommended: limit the app to this one mailbox with Exchange
							<span class="font-medium">RBAC for Applications</span>. Without it, the permission covers
							every mailbox in your organization.
						</div>
					</li>
					<li class="flex gap-3">
						<span class="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-muted text-xs font-semibold text-muted-foreground">3</span>
						<div>
							Enter the account above and press <span class="font-medium">Test</span>. It only reads; nothing is
							created. Permission changes can take up to an hour to reach Microsoft's servers.
						</div>
					</li>
				</ol>
			</div>
		{/if}
	</div>

	<ConfirmDialog
		bind:open={confirmOff}
		title="Turn off the workspace Teams account?"
		description="New Teams bookings will only get a link from a host's own Microsoft work account or the event type's fixed link. Meetings already booked keep working, and reschedules and cancellations still reach them."
		confirmText="Turn off"
		destructive
		onConfirm={() => save('')}
	/>
{/if}
