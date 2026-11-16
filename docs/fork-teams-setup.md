# Setting up Microsoft Teams meetings (wisegrid fork)

How the production instance (`https://appointments.wisegridenergy.com`) is set up to put
Microsoft Teams links on bookings. No code is involved: Calnode already asks Microsoft Graph
for a Teams meeting when it creates the calendar event (`internal/calendar/microsoft/events.go`,
`isOnlineMeeting` + `teamsForBusiness`). Everything below is Microsoft-side configuration plus
three environment variables.

Set up 2026-11-16. Account names, tenant IDs and secrets are deliberately left out of this
public repo; they are with the company's Microsoft administrator.

## The model: one shared Microsoft account, hosts stay on Google

Every WiseGrid host's main calendar is Google. Teams needs a Microsoft **work** account
with a mailbox, so the company has one shared appointments account in its own Microsoft 365
organization. Each host who offers Teams connects that account in Calnode **as a second
calendar**, next to their Google one.

- `Service.MeetingProvider` (`internal/calendar/calendar.go`) looks past the host's
  destination calendar: a Teams booking is created on the shared Microsoft calendar, a Meet
  booking on the host's Google calendar.
- The Microsoft event invites the host (their Google address) and the booker, so both get
  the Teams link.
- A new connection never takes over "Add bookings here" from an existing one
  (`connstore.ResolveFlags`), so Google stays the destination.

An earlier attempt pointed at a different Microsoft organization that had no license, so it
could not create meetings. It was replaced, not repaired.

**Who can connect.** The app is single-tenant, so only accounts in the company's Microsoft 365
organization can connect. Microsoft also only lets licensed work accounts create Teams
meetings. A host's Gmail or personal Outlook account will never work. The options considered
were: (A) a licensed account per host, (B) one shared account, and (C) a multi-tenant app for
hosts who already have work accounts elsewhere. **B was chosen**, for cost. Switching a host to A
later needs no code: give them their own licensed user, and they connect that instead.

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
  `offline_access`, `openid`, `profile`, `email` → **Grant admin consent**.

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

## 5. Each host (Calnode admin → Calendar)

1. **Connect Microsoft 365 (Outlook)** and sign in as the shared account.
2. Leave **Add bookings here** on the Google calendar.
3. On the Microsoft connection → **Manage calendars** → **untick Check** on its calendar, and save.

Step 3 matters when more than one host shares the account. Otherwise every Teams booking on the
shared calendar shows as busy for *all* of them, and they block each other's slots. A fully
unticked account is skipped by free/busy entirely (`freeBusyConnections`), so an expired
Microsoft token also can't break those hosts' availability.

## 6. Event types

Set Location to **Microsoft Teams**, or keep Google Meet and tick **Let the booker choose Google
Meet or Microsoft Teams**.

## 7. Test

Book from a private window with an outside address (e.g. Gmail). Then check that:

- the confirmation email has a `teams.microsoft.com` join link;
- the event is on the shared account's Outlook calendar, with host and booker invited;
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
(yet). Calnode did its part: the booking's `booking_hosts.external_provider` is `microsoft` with
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
