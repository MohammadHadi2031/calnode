package handler

import (
	"context"
	"database/sql"
	"fmt"
)

// forkEventTypeFields are the event-type settings this fork adds (migration 90001),
// embedded in the create and update request bodies. Pointers: an omitted field keeps the
// stored value (or the column default on create).
type forkEventTypeFields struct {
	MaxGuests *int `json:"max_guests"`
	// ConferenceOptions (migration 90006) are the video platforms offered to the booker
	// beside the location type. AllowConferenceChoice is its pre-list form, still accepted
	// from older clients: true means Meet and Teams. ConferenceOptions wins when both are sent.
	ConferenceOptions     *[]string `json:"conference_options"`
	AllowConferenceChoice *bool     `json:"allow_conference_choice"`
	InviteTimezone        *string   `json:"invite_timezone"`
}

// conferenceOptions is the stored form of the request's platform list, nil when the
// request does not set it.
func (f forkEventTypeFields) conferenceOptions() *string {
	var enc string
	switch {
	case f.ConferenceOptions != nil:
		enc = encodeConferenceOptions(*f.ConferenceOptions)
	case f.AllowConferenceChoice != nil:
		if *f.AllowConferenceChoice {
			enc = "google_meet,teams"
		}
	default:
		return nil
	}
	return &enc
}

// validate returns a client-facing message for an invalid field, or "". These are plain
// range/enum checks that hold for any stored row (the CHECK constraints guarantee it),
// so validating on mention cannot strand an event type the way CLAUDE.md warns about.
func (f forkEventTypeFields) validate() string {
	if f.MaxGuests != nil && (*f.MaxGuests < 0 || *f.MaxGuests > maxGuestsCeiling) {
		return fmt.Sprintf("max_guests must be between 0 and %d", maxGuestsCeiling)
	}
	if f.ConferenceOptions != nil {
		for _, p := range *f.ConferenceOptions {
			if !conferencePlatform(p) {
				return "conference_options may only list google_meet, teams and zoom"
			}
		}
	}
	if f.InviteTimezone != nil {
		switch *f.InviteTimezone {
		case "attendee", "host", "utc":
		default:
			return "invite_timezone must be 'attendee', 'host' or 'utc'"
		}
	}
	return ""
}

// each hands every provided field to set (the PATCH handler's column setter).
func (f forkEventTypeFields) each(set func(col string, val any)) {
	if f.MaxGuests != nil {
		set("max_guests", *f.MaxGuests)
	}
	if opts := f.conferenceOptions(); opts != nil {
		set("conference_options", *opts)
		set("allow_conference_choice", *opts != "") // kept in step for a rollback
	}
	if f.InviteTimezone != nil {
		set("invite_timezone", *f.InviteTimezone)
	}
}

// apply writes the provided fields onto a just-inserted event type inside the create
// transaction, so the upstream INSERT stays untouched.
func (f forkEventTypeFields) apply(ctx context.Context, tx *sql.Tx, id string) error {
	var err error
	f.each(func(col string, val any) {
		if err != nil {
			return
		}
		_, err = tx.ExecContext(ctx, `UPDATE event_types SET `+col+` = ? WHERE id = ?`, val, id) // #nosec G202 -- col is one of the literals in each()
	})
	return err
}
