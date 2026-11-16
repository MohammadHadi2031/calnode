-- +goose Up
-- Fork migration (wisegrid): the booker's choice of video platform becomes a list, so
-- Zoom can be offered next to Google Meet and Microsoft Teams. conference_options holds
-- the platforms the booker may pick from, comma-separated (google_meet, teams, zoom);
-- the event type's location_type is the preselected one and is always offered. '' = no
-- choice. allow_conference_choice is kept in step for a rollback; the code no longer
-- reads it.
ALTER TABLE event_types ADD COLUMN conference_options TEXT NOT NULL DEFAULT '';
UPDATE event_types SET conference_options = 'google_meet,teams'
WHERE allow_conference_choice = 1 AND location_type IN ('google_meet', 'teams');

-- +goose Down
ALTER TABLE event_types DROP COLUMN conference_options;
