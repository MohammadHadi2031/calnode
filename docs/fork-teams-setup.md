# Setting up Microsoft Teams meetings (wisegrid fork)

How the production instance (`https://appointments.wisegridenergy.com`) is set up to put
Microsoft Teams links on bookings: Microsoft-side configuration, three environment variables,
and one setting in Calnode (Settings → Microsoft Teams).

Set up 2026-11-16. Account names, tenant IDs and secrets are deliberately left out of this
public repo; they are with the company's Microsoft administrator.

## The model: one workspace Teams account, hosts stay on Google

Every WiseGrid host's main calendar is Google. Teams needs a Microsoft **work** account
with a mailbox, so the company has one shared appointments account in its own Microsoft 365
organization. An admin enters it once in **Settings → Microsoft Teams**. Hosts connect nothing.

- Calnode reaches the mailbox **as the app itself** (OAuth client credentials,
  `internal/calendar/microsoft/workspace.go`), never as a signed-in user. Nobody needs the
  account's password or MFA device, and there is no user token to expire, be revoked by a
  password change, or be disconnected by one host for everyone. The only credential is the
  client secret, which every Microsoft feature already depends on.
- For a Teams booking, `Service.MeetingProvider` (`internal/calendar/calendar.go`) uses a
  host's **own** Microsoft work account if they connected one, and otherwise the workspace
  account. Bookings it organizes are stamped `microsoft_workspace` with the mailbox as their
  calendar id, so reschedules and cancels find them even after the setting changes.
- The event lives on the shared mailbox and invites the booker and every host (their Google
  addresses), so everyone gets the Teams link and it lands on each host's own calendar.
- The workspace account is **never checked for conflicts** and is never anyone's "Add
  bookings here" calendar. Two hosts can have Teams meetings at the same time: Microsoft does
  not refuse overlapping events, and nobody blocks anybody's slots.
- Reassigning a booking to another host swaps the hosts on the existing event instead of
  deleting and recreating it, which would cancel the Teams meeting the booker already has.

**Limits of a shared organizer.** Hosts join as guests from outside the organization, so
nobody in the meeting has organizer rights: no recording or transcription, and limited power
to remove people or end the meeting for everyone. All meeting chats belong to the shared
account. A host who needs those gets their own licensed account and connects it on the
Calendar page; they then organize their own meetings and nothing else changes.

**History.** Until Nov 2026 each host connected the shared account on their Calendar page and
unticked its conflict check. That needed the password on every host's machine, and a forgotten
untick silently cost a host slots. Those per-host connections are now ignored for Teams once
the workspace account is set (`MeetingProvider` skips a connection to the workspace mailbox),
and can simply be disconnected.

**Who can be the workspace account.** The app is single-tenant, so it must be a user in the
company's Microsoft 365 organization, licensed for Exchange Online and Teams. `MICROSOFT_TENANT`
must name that organization (not `common`), since client credentials are issued by one tenant.

## 1. License (admin.cloud.microsoft)

Users → Active users → the appointments account → Licenses and apps. Both **Exchange Online**
and **Microsoft Teams** must be ticked. A Teams-only license has no mailbox, and Graph cannot
create calendar events (or the Teams meeting on them) without one.

## 2. App registration (entra.microsoft.com → App registrations)

- Name `Calnode`, **single tenant** (this organizational directory only).
- Redirect URIs, platform **Web**, both:
  - `https://appointments.wisegridenergy.com/v1/calendar/callback` (connect a calendar)
  - `https://appointments.wisegridenergy.com/v1/auth/microsoft/callback` (Sign in with Microsoft)
- Certificates & secrets → new client secret, 24 months. Copy the **Value**, not the Secret ID.
- API permissions → Microsoft Graph → **Delegated**: `Calendars.ReadWrite`, `User.Read`,
  `offline_access`, `openid`, `profile`, `email` (hosts with their own account, and Sign in
  with Microsoft).
- API permissions → Microsoft Graph → **Application**: `Calendars.ReadWrite` (the workspace
  account) → **Grant admin consent**.
- Recommended: limit the application permission to the appointments mailbox with Exchange
  **RBAC for Applications**. Without it, it covers every mailbox in the organization.

**Secret expiry:** the secret expires 24 months after it was created. When it does, Teams
links stop being created and hosts get "reconnect needed". Create a new secret, update `.env`,
recreate the container. Nothing else changes.

## 3. Lobby policy (admin.teams.microsoft.com)

Meetings → Meeting policies → **Global (Org-wide default)**:

- **Who can bypass the lobby:** Everyone
- **Anonymous users can join a meeting:** On

This is required for this setup, not a preference. The meeting organizer is the shared account
and every host joins from Google, so to Teams *everyone* is outside the organization. On the
default policy hosts and leads would all wait in the lobby with nobody inside to admit them.
See `docs/fork-teams-lobby.md` for why Calnode does not set this per meeting. Policy changes can
take a few hours to apply.

## 4. Server

In the deployment's `.env`:

```
MICROSOFT_CLIENT_ID=<Application (client) ID>
MICROSOFT_CLIENT_SECRET=<secret Value>
MICROSOFT_TENANT=<tenant>.onmicrosoft.com   # or the Directory (tenant) ID
```

Then `docker compose up -d` (a plain restart does not reload `.env`). The log must show both
`Microsoft OAuth login configured` and `Microsoft 365 calendar configured` with the right tenant.

Check the tenant spelling: a typo still logs "configured" and only fails when someone signs in.
To check it without signing in:

```
curl -s https://login.microsoftonline.com/<tenant>/v2.0/.well-known/openid-configuration
```

It returns JSON for a real tenant and `AADSTS90002 ... not found` for a typo. A client-credentials
token request against the same tenant confirms the client ID and secret together. Redirect URIs
can only be confirmed by a real sign-in: Microsoft answers every unauthenticated request with
50058 before it looks at the redirect URI.

## 5. Workspace Teams account (Calnode admin → Settings → Microsoft Teams)

Enter the appointments account's address, **Save**, then **Test**. Test only reads: it asks
Microsoft for a token, reads the mailbox's calendar, and checks that `teamsForBusiness` is
among its allowed meeting providers, which is the condition behind "event but no Teams link"
below. Each failure says what to fix (secret, permission, mailbox, license). Saving does not
require Test to pass: until it does, bookings fall back to a host's own account or the event
type's fixed link.

Hosts who connected the shared account on their Calendar page before this existed can
disconnect it. It is no longer used for Teams.

## 6. Event types

Set Location to **Microsoft Teams**. To let the booker choose, tick the other platforms under
**Booker can also choose** (Google Meet, Zoom, and for Meet/Teams a phone call). The location
is preselected. Only platforms Calnode can create a link for can be newly ticked.

## 7. Test

Book from a private window with an outside address (e.g. Gmail). Then check that:

- the confirmation email has a `teams.microsoft.com` join link;
- the event is on the shared account's Outlook calendar, with host and booker invited;
- on the server, the booking's `booking_hosts.external_provider` is `microsoft_workspace`;
- both the host and the booker get straight into the meeting, without waiting in the lobby.

## Troubleshooting

Each of these was hit during the first setup (2026-11-16).

**"Selected user account does not exist in tenant … and cannot access the application …"**
The account picked on Microsoft's screen is not in the organization. Usually it's a cached
personal account, or an account from the old organization. Use a private window and choose
"Use another account". Type the full `…@<tenant>.onmicrosoft.com` address, not an
`@wisegridenergy.com` one. Hosts keep logging in to Calnode with Google. The Microsoft account is
only connected from **Calendar**, never through "Sign in with Microsoft" on the login page.

**The booking has an Outlook event but no Teams link, and nothing in the log.** Microsoft
accepted the event and silently dropped the Teams meeting, because the account can't create one
(yet). Settings → Microsoft Teams → **Test** reports exactly this ("not ready for Teams yet"). Calnode did its part: the booking's `booking_hosts.external_provider` is `microsoft` with
an event ID, and `bookings.location_value` is empty. Confirm by scheduling a meeting by hand at
teams.microsoft.com as the same account. If that fails too, it's on Microsoft's side, not Calnode's.
Another sign: Outlook on the web has no Teams meeting toggle on a new event. For a definite
answer, run this in Graph Explorer as that account:
`GET https://graph.microsoft.com/v1.0/me/calendar?$select=allowedOnlineMeetingProviders`.
`teamsForBusiness` must be listed.

Causes, most likely first:

1. **Still provisioning.** A newly licensed user can sign in to Teams but can't schedule for up
   to ~24 hours. Teams just says "Something went wrong".
2. **Meeting policy** (Teams admin center → Meetings → Meeting policies → Global): *Private
   meetings scheduling*, *Outlook add-in* and *Channel meetings scheduling* must be On. Also, under
   Teams apps → Permission policies, Microsoft apps (Calendar in particular) must be allowed.
3. **Teams upgrade mode** (Users → Manage users → the account): must be *Teams only*.
4. **A "(no Teams)" license.** Since 2024 many Microsoft 365 plans are sold without Teams. Check
   Billing → Your products for the exact name. Teams then needs a separate *Microsoft Teams
   Enterprise* license.

Still failing after 24 hours with all of the above correct: open a ticket in the Microsoft 365
admin center → Help & support.

**Teams says the account doesn't exist.** Usually it's the wrong Teams: the built-in Windows
"Microsoft Teams (free)" app and `teams.live.com` only take personal accounts. Use
`https://teams.microsoft.com`. If that also refuses, see causes 1 and 4 above. Teams admin
center → Users → Manage users only lists the account once Teams has provisioned it.

## Status

2026-11-16: Calnode side verified. The tenant, client ID and secret were checked against
Microsoft from the server, and a test booking wrote the event to the shared calendar through
the Microsoft connection. Teams links are blocked on Microsoft's side: the shared account cannot
schedule a Teams meeting even by hand in Teams ("Something went wrong"). Next: once Teams
can schedule by hand, make a fresh test booking and confirm the link appears.

Later 2026-11-16: the workspace Teams account replaced per-host connections (code). To roll
out: add the Application `Calendars.ReadWrite` permission + admin consent (§2), deploy, enter
the account in Settings → Microsoft Teams (§5), press Test, then make a test booking (§7).
