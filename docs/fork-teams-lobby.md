# Teams meetings and the lobby (wisegrid fork)

This documents a known limit on checklist requirement R2: Calnode does not set the lobby
bypass on Microsoft Teams meetings it creates. It was a deliberate decision on 2026-10-31.

## What happens today

When a booking uses Microsoft Teams, Calnode creates the Teams meeting through the host's
calendar event (Graph `isOnlineMeeting` with `teamsForBusiness`). The meeting uses the
host's organization's **default Teams lobby policy**. Calnode does not change it.

| Who joins | Result |
| --- | --- |
| A colleague from the same Microsoft organization as the organizer | Joins directly. This is Microsoft's default. |
| An external lead (any other organization, Gmail, etc.) | Waits in the lobby until someone in the meeting admits them. |

The default can differ if your Microsoft administrator changed the tenant's meeting policy.
Check Teams admin center → Meetings → Meeting policies → "Who can bypass the lobby".

## What hosts should do

- **Join early.** A host who is already in the meeting admits the lead as soon as they arrive. Every internal host can do this, not only the organizer.
- **Prefer Google Meet where a host has Google connected.** People invited on the Google Calendar event, including the lead, join Meet without knocking. Calnode already puts the booker, guests and co-hosts on the one shared event.
- **Teams links need a work or school Microsoft account.** A personal Microsoft account cannot create them (ARCHITECTURE §18).

## Why it was not built

Setting the lobby bypass on the meeting needs a second Microsoft permission,
`OnlineMeetings.ReadWrite`, on top of the `Calendars.ReadWrite` the app asks for now
(`internal/calendar/microsoft/microsoft.go`). That would mean:

- every Microsoft-connected host reconnects their calendar once;
- the permission added to the app registration in Azure, and possibly one-time approval by the tenant administrator;
- a standalone Teams meeting created through the `onlineMeetings` API and its link attached to the calendar event, instead of the current single call.

Setting the bypass to "everyone" would also let external leads skip the lobby, which is a
policy choice for the business. Setting it to "organization" adds nothing over today's default.

## If this becomes a problem

Revisit if leads regularly wait unadmitted. The change would be confined to the Microsoft
provider's `CreateEvent`; the booking flow, emails and reminders already carry whatever join
link the provider returns.
