# Hosts add guests to a booking (wisegrid fork)

A host (or an admin) can add a guest to an upcoming booking, and the guest is invited
straight away. This covers requirement R4 in the booking system checklist. There is no
migration: guests are `booking_attendees` rows with `is_organizer = 0` (migration 90001),
just like the guests a booker adds.

## Who and when

- **Who:** admins, on every booking, and any host with a seat on the booking. This is the same rule as the bookings list and the email log (`canManageBooking`).
- **When:** confirmed bookings that have not ended yet.
- **Refused:**
  - the booker's address;
  - a host's address;
  - someone already invited;
  - more than 50 guests on one booking.

  The event type's `max_guests` applies to bookers, not to hosts.
- **Zone and language:** the guest takes the booker's time zone and language, the same as guests added at booking time.

## How the guest is invited

It depends on the booking's invite mode (`bookings.invite_delivery`):

- **Calendar-sent (default).** The guest is added as an attendee to every event the booking holds:
  - the single shared event on the organizer host's calendar, or
  - each host's own event.

  The provider then emails the invite. This is the same path the other guests went through, so Google Meet admits the new guest without knocking. Both providers re-send the full attendee list, keeping existing attendees and their RSVP state, because their PATCH replaces the list:
  - Google: `AddAttendees` patches with `sendUpdates=all`.
  - Microsoft Graph: the PATCH notifies the new attendee.
- **Fallback.** Calnode emails the guest their own confirmation with the `.ics` when the booking has no provider event, or when any event cannot take the attendee. That covers CalDAV, which has no `AttendeeAdder`, a disconnected host, and an API error. An invite from both is better than none.
- **Calnode-sent.** Calnode emails the guest their confirmation with the `.ics`. It carries the booking's UID, so the later reschedule or cancellation `.ics` updates the same calendar entry.

The guest is added even if the invite fails. The response then carries a `warning`, and the
Emails log shows the failed attempt, which can be resent.

From then on, reschedules and cancellations reach the guest like any other guest, because those
paths read the booking's guest list.

## API and UI

| Endpoint | |
| --- | --- |
| `GET /v1/bookings/{id}/guests` | `{items: [{name, email}]}` |
| `POST /v1/bookings/{id}/guests` with `{"email", "name"?}` | `201 {guest, guests, invited_via: "calendar"\|"email", warning?}` |

On the Bookings page, the expanded booking lists its **Guests**. For an upcoming booking it
has an email and name field with an **Add guest** button.

## Not covered

- Removing a guest.
- On the calendar path, the booker's own invitation is not re-sent with the longer guest list. The provider event shows the full list to everyone who opens it.
