# Google invites in the booker's time zone (wisegrid fork)

Google's own invitation email ("Invitation: … @ Mon Nov 2, 2026 09:30 - 10:00 (GMT+3:30)")
shows the time in the **organizer's calendar time zone**. Writing the event in the booker's
zone (`invite_timezone`, migration 90001) does not change that, and Google's API has no
setting for it. A booker in another country therefore read the host's local time.

The fork stops Google from emailing anyone. Calnode sends the invitation instead, in the
booker's language and time zone (see [fork-email-timezones.md](fork-email-timezones.md)).
The attached `.ics` *is* the Google event.

## What happens

| Step | Google | Calnode |
| --- | --- | --- |
| Booking | Event created with booker, guests and co-hosts as attendees, `sendUpdates=none` | Confirmation email with `.ics` REQUEST to the booker and each guest |
| Reschedule | Event moved, `sendUpdates=none` | Reschedule email with `.ics` REQUEST (new time, Google's new SEQUENCE) |
| Cancel | Event deleted, `sendUpdates=none` | Cancellation email with `.ics` CANCEL |
| Host adds a guest | Guest added to the event, `sendUpdates=none` | Guest gets the confirmation email with the `.ics` |
| Reassign | Old event deleted; new event created with the **old event's iCalUID** | Booker gets the new invite, which replaces their existing entry |
| Resend confirmation | — | Same `.ics` as the original |

The `.ics` carries the Google event's own `UID` (iCalUID), `SEQUENCE` and `ORGANIZER`
(the host's Google calendar). It is read with `events.get` just before each send. As a result:

- the booker's calendar entry and the Google event are the same meeting;
- Accept/Decline goes to the host's Google calendar and updates the attendee status there;
- if the host later edits the event in Google and lets Google notify guests, the booker's entry updates instead of being duplicated;
- the booker and guests are still attendees of the event, so Google Meet lets them in without knocking.

## Hosts

- The host whose Google calendar holds the event gets no `.ics`: the event is already there.
- A co-host on Google sees the shared event on their calendar as an attendee, with no `.ics`.
- A co-host whose bookings go to **Outlook or CalDAV** used to get Google's invite. Now their
  host email carries the shared event's `.ics`, listing them as an attendee. This also covers
  the organizer when the Meet was minted through a Google account that is not their destination.

## Limits

- **Attendee emails off.** If the primary host turned off attendee confirmation,
  reschedule or cancellation emails (Settings → Notifications), that step sends no
  invite at all. This is the same as "Sent by: Calnode" and as hosts without Google.
- **Email must be configured.** Invites now go out through Calnode's mailer (Settings → Email).
- **Microsoft is unchanged.** Graph always emails attendees itself, and its invitation also
  uses the organizer's zone. Teams events keep the provider's invite.
- **Fallback.** If reading the Google event's identity fails, the `.ics` falls back to
  Calnode's own UID (`<booking>@calnode`) with the host as organizer. The meeting still
  reaches the booker's calendar, but it is not linked to the Google event. The failure is logged.
- **Old bookings.** Bookings made before this change were invited by Google. A reschedule or
  cancel now sends Calnode's `.ics` with the same Google UID and a newer SEQUENCE. Mail clients
  match it to the entry Google created, so the booker's calendar still updates.

## Code

- `internal/gcal`: `quietUpdates` (`sendUpdates=none` on every write), `InvitesGuests()` returns false, `EventInvite`, and `CreateEvent` honouring `ICalUID`/`Sequence` with a retry without them.
- `internal/calendar`: `QuietInviter`, `EventInvite`, `Service.EventInvite`, `Service.ProviderPlacesGuests`; `AddAttendees` also reports whether the provider sent the invite.
- `internal/mailer`: `BookingData.ICSUID`, `ICSOrganizerName/Email`, `ICSHostAttendee`.
- `internal/handler/provider_invite.go`: `providerEventInvite`, `apply`, `applyHostProviderInvite`. These are wired into confirmation, reschedule, cancel, resend, add-guest and reassign. `hostCalendarLacksEvent` keeps Google hosts from getting an `.ics` for their own event.
