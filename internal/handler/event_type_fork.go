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
	MaxGuests             *int    `json:"max_guests"`
	AllowConferenceChoice *bool   `json:"allow_conference_choice"`
	InviteTimezone        *string `json:"invite_timezone"`
}

// validate returns a client-facing message for an invalid field, or "". These are plain
// range/enum checks that hold for any stored row (the CHECK constraints guarantee it),
// so validating on mention cannot strand an event type the way CLAUDE.md warns about.
func (f forkEventTypeFields) validate() string {
	if f.MaxGuests != nil && (*f.MaxGuests < 0 || *f.MaxGuests > maxGuestsCeiling) {
		return fmt.Sprintf("max_guests must be between 0 and %d", maxGuestsCeiling)
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
	if f.AllowConferenceChoice != nil {
		set("allow_conference_choice", *f.AllowConferenceChoice)
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
		_, err = tx.ExecContext(ctx, `UPDATE event_types SET `+col+` = ? WHERE id = ?`, val, id) // #nosec G202 -- col is one of the three literals in each()
	})
	return err
}
