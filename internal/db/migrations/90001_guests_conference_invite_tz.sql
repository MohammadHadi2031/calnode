-- +goose Up
-- Fork migration (wisegrid): additional guests, attendee-chosen conference platform,
-- one shared invite for multi-host bookings, and the time zone of provider invites.
-- Numbered 90001+ so it never collides with upstream's sequence (see db.Migrate).

-- How many additional guest emails a booker may add (0 = guests disabled).
ALTER TABLE event_types ADD COLUMN max_guests INTEGER NOT NULL DEFAULT 10
    CHECK (max_guests BETWEEN 0 AND 50);

-- For a Google Meet or Teams event type: let the booker pick either platform. The link is
-- minted by the first assigned host whose connected calendar can create that platform.
ALTER TABLE event_types ADD COLUMN allow_conference_choice INTEGER NOT NULL DEFAULT 0;

-- Time zone the provider (Google) invite is written in, which is the zone its invitation
-- email renders times in for recipients without their own Google calendar setting.
ALTER TABLE event_types ADD COLUMN invite_timezone TEXT NOT NULL DEFAULT 'attendee'
    CHECK (invite_timezone IN ('attendee', 'host', 'utc'));

-- A multi-host booking whose invites go through the hosts' calendars gets ONE event, on
-- the organizer host's calendar, with every other host and every guest as an attendee.
-- Those other hosts' seats hold no event of their own; this marks them, so the reconciler
-- does not "heal" them with a duplicate event and own-event exclusion still recognises
-- the invited copy on their calendar.
ALTER TABLE booking_hosts ADD COLUMN invited_via_organizer INTEGER NOT NULL DEFAULT 0;

-- Guests are booking_attendees rows with is_organizer = 0.
CREATE INDEX IF NOT EXISTS ix_booking_attendees_booking ON booking_attendees (booking_id);
CREATE UNIQUE INDEX IF NOT EXISTS ux_booking_attendees_email
    ON booking_attendees (booking_id, email COLLATE NOCASE);

-- +goose Down
DROP INDEX IF EXISTS ux_booking_attendees_email;
DROP INDEX IF EXISTS ix_booking_attendees_booking;
ALTER TABLE booking_hosts DROP COLUMN invited_via_organizer;
ALTER TABLE event_types DROP COLUMN invite_timezone;
ALTER TABLE event_types DROP COLUMN allow_conference_choice;
ALTER TABLE event_types DROP COLUMN max_guests;
