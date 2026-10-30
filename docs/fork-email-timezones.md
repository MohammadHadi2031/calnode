# Time zones in emails (wisegrid fork)

Every booking email names its time zone and shows a second zone alongside, so nobody
converts times by hand. This covers requirement R5 in the booking system checklist.

| Reader | Times shown in | Second zone in brackets |
| --- | --- | --- |
| Booker and guests (their language) | the booker's zone, as picked on the booking page | UTC |
| Each host (English) | the host's own zone (`users.iana_timezone`, Settings → Profile) | the booker's zone |
| A host with no zone on file | the booker's zone | UTC |

Examples:

- To the booker: `Thu 1 Oct 2026, 7:30 PM – 8:00 PM Tehran (4:00 PM – 4:30 PM UTC)`
- To an Istanbul host: `Thu 1 Oct 2026, 7:00 PM – 7:30 PM Istanbul (7:30 PM – 8:00 PM Tehran)`

## Rules

- **Labels.** A zone is labelled by the city in its IANA name ("Tehran", "New York"), or "UTC". It is never an abbreviation: Go has none for many of the zones we use, and Tehran, Istanbul and Amman used to print as `+0330` or `+03`.
- **No repeats.** The second zone is left out when it shows the same clock time as the first, for example a UTC booker, or Istanbul and Amman (both +03).
- **Different day.** When the second zone falls on another day, it carries its own date.
- **DST.** Each instant is converted with its own offset, so DST is correct on either side of a change. This includes a reschedule across one, where the old and new times each use their own offset.
- **Unknown zones.** An unknown zone name falls back to UTC rather than failing the email.
- **Container.** The IANA database is embedded in the binary (`time/tzdata`), so the container needs no tzdata.

Code: `BookingData.zones`, `fmtAt`, `WhenFmt` and `zoneName` in `internal/mailer/booking.go`.
The host senders set `hostView`. Callers fill `HostTimezone`: `hostBookingData`, the
reschedule loop, `loadHostIntoData` (used by reassign) and the reminder worker.
