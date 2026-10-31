# Staff calendar, unavailable blocks and secretaries (wisegrid fork)

The admin **Schedule** page is a week/day calendar. On it you can:
- drag over a time range to create an appointment or an unavailable block;
- drag or resize an appointment to move it;
- cancel an appointment.

Who can use it:
- **Hosts** see and manage their own calendar.
- **Secretaries** manage the hosts an admin assigned to them.
- **Admins** manage every host.

Fork migration: `90002_staff_calendar.sql`.

## Who can manage whom

`canManageHost(actor, host)` is true when any of these holds:
- `actor == host`;
- `actor.is_admin`;
- `secretary_hosts(actor, host)` exists.

A secretary is any user with at least one `secretary_hosts` row. Admins edit these rows from the Members page. A secretary can still be a normal host.

## Unavailable blocks

- `unavailable_blocks(id, user_id, start_at, end_at, title, sync_to_calendar, external_*)` stores time ranges in UTC.
- A block removes the host's slots from the public booking page. `hostAvailableAt` treats a block as being outside working hours, and slot generation counts it as busy.
- With `sync_to_calendar`, Calnode also writes a busy event with no attendees to the host's destination calendar. It moves that event when the block changes and deletes it when the block is deleted.
- Turning the switch off removes the external event.

## API

All endpoints need auth. Times are RFC3339; the server returns UTC.

| Method & path | Who | Body / query | Result |
|---|---|---|---|
| `GET /v1/users/me` | any | | adds `secretary_for: [host_id]` |
| `GET /v1/staff/hosts` | any | | `{hosts:[{id,name,email,timezone,avatar_url,self,has_calendar,event_types:[{id,slug,name,duration_minutes,location_type,allow_conference_choice}]}]}`: the hosts the caller can manage, with their active event types |
| `GET /v1/staff/calendar` | any | `from`, `to` (RFC3339, at most 62 days apart), `host_ids` (comma list; default: every manageable host), `external=1` to include calendar busy | `{bookings:[…], unavailable:[…], busy:[…], busy_errors:[…]}`, see below |
| `POST /v1/staff/bookings` | manager of `host_id` | `{host_id,event_type_id,start_at,end_at?,name,email,timezone?,guests?:[email \| {name,email}],conference?,force?}` | `201` booking JSON; `409 {error:"conflicts",conflicts:[…]}` unless `force` |
| `PATCH /v1/staff/bookings/{id}` | manager of the booking's primary host | `{start_at,end_at?,force?}` (`end_at` defaults to start + the current length) | `200` booking JSON; `409` conflicts unless `force` |
| `POST /v1/staff/bookings/{id}/cancel` | manager of the primary host | `{reason?}` | `200` booking JSON |
| `POST /v1/unavailable-blocks` | manager of `host_id` | `{host_id,start_at,end_at,title?,sync_to_calendar?:true,force?}` | `201` block; `409` conflicts (an overlapping booking) unless `force` |
| `PATCH /v1/unavailable-blocks/{id}` | manager | `{start_at?,end_at?,title?,sync_to_calendar?,force?}` | `200` block |
| `DELETE /v1/unavailable-blocks/{id}` | manager | | `204` |
| `GET /v1/secretaries` | admin | | `{assignments:[{secretary_id,host_id}]}` |
| `PUT /v1/users/{id}/secretary-hosts` | admin | `{host_ids:[…]}` (replaces the set; drops the user's own id) | `{host_ids:[…]}` |

Calendar items:

```
booking:     {id, event_type_id, event_type_name, primary_host_id, host_ids:[…], start_at, end_at,
              status, location_type, location_value, attendee_name, attendee_email,
              attendee_timezone, guests:[email], guest_list:[{name,email}], can_manage}
unavailable: {id, host_id, start_at, end_at, title, sync_to_calendar, synced}
busy:        {host_id, start_at, end_at}      // external calendar busy, minus Calnode's own events and blocks
busy_errors: {host_id, error}   // error says why: no mailbox, consent needed, network…
```

Conflicts (`409`):

```
{error:"conflicts", conflicts:[{host_id, host_name,
  kind: "past" | "outside_hours" | "booking" | "unavailable" | "calendar" | "calendar_unchecked",
  detail}]}
```

`past` and `same_start` can't be overridden. Staff can't book or move into the past, and the `idx_bookings_no_double` index refuses a second live booking with the same primary host and the same start, even with `force`. The client shows any other conflict as a warning and resends the request with `force: true`.

Staff bookings follow the event type's routing, with three differences:
- In round-robin, the chosen host replaces the rotation pick.
- Min-notice, max-future, the per-email limits and intake questions are skipped.
- `force` also skips the double-booking check.

Confirmations, invites, Meet/Teams links and reminders work the same as for a public booking.

## Time zones on the page

- The calendar is drawn in the viewer's profile zone. The picker in the header switches the view to any IANA zone for this browser; "Use my zone" switches back. The header sentence and the grid's top-left corner always name the zone in use.
- The new-appointment dialog has its own **time zone**, which starts as the view's zone. Start and end are entered in it, and it is sent as the booking's `timezone`, so the attendees' emails and invites use it. Changing it keeps the selected instant and re-expresses the times. Below the times, one line per distinct zone shows the attendees', the viewer's and the host's local time. Offsets are taken on the appointment's date, so DST is right.
- The appointment popup shows the same lines from `attendee_timezone` and each host's zone.

## Attendees

The dialog has one **Attendees** list with **Add attendee**. The first person is the main contact: they are the booking's organizer and get the confirmation with the reschedule and cancel link. Everyone after them is sent as a guest with their name. The popup lists everyone the same way and offers **Add attendee** on upcoming confirmed appointments the viewer manages. That button calls `POST /v1/bookings/{id}/guests` (docs/fork-host-guests.md), which a secretary of the booking's primary host may now use as well.
