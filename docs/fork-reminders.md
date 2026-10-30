# Reminders to everyone on a booking (wisegrid fork)

Reminders used to go to the booker only, and without a link to reschedule. They now reach
everyone on the booking. This covers requirement R6 in the booking system checklist.

Fork migration: `90004_host_reminder.sql`.

## Who gets the reminder

Reminders run as `reminder.send` jobs, one per `event_type_reminders.hours_before`, for
example 24 h and 1 h before the start. Each run sends to:

| Recipient | Language | Includes | Gated by |
| --- | --- | --- | --- |
| Booker | booker's | conference link, add-to-calendar links, and a **fresh manage link** to reschedule or cancel | the primary host's "Reminder emails" switch (Settings → Notifications → emails sent to attendees), as before |
| Each guest | booker's (guests share it) | conference link; no manage link, since guests cannot reschedule | same switch |
| Each host on the booking (`booking_hosts`) | English | event, booker and guests, time, location | that host's own **Upcoming meeting reminder** switch (`users.notify_host_reminder`, on by default) |

Only a hash of the booking-time manage token is stored, so each reminder issues a new
token, as a resend does.

## Failures

The booker is sent first, and only a failure there fails the job. The worker then retries
the whole job and nobody has had a reminder yet, so nobody gets a duplicate. A failure to
a guest or a host is logged instead of retried. Every attempt is in the booking's email
log, and an undelivered one raises the usual alert (see fork-email-log.md).

## Changed behaviour

- Turning off the attendee "Reminder emails" switch no longer silences the hosts' reminders.
- A host of a LiveKit booking gets the attendee join link in the reminder. Their host-controls link is in their confirmation email.
