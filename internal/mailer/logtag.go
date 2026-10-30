package mailer

import (
	"context"
	"errors"
)

// ErrNotConfigured is what the email log records for a booking email sent while no
// mail transport is configured: the Noop mailer accepted it and delivered nothing.
var ErrNotConfigured = errors.New("email is not configured (Settings → Email)")

// LogTag says which booking email a Send belongs to, so a logging Mailer can record
// it against the booking (wisegrid fork, see docs/fork-email-log.md). The typed Send*
// helpers in booking.go set it; a Send without a tag (magic link, password reset, test
// email) is not a booking email and is not logged.
type LogTag struct {
	BookingID string
	Kind      string // confirmation | cancellation | reschedule | reminder
	Role      string // attendee | guest | host
}

type logTagKey struct{}

// WithLogTag returns ctx carrying t.
func WithLogTag(ctx context.Context, t LogTag) context.Context {
	return context.WithValue(ctx, logTagKey{}, t)
}

// LogTagFrom returns the tag set by WithLogTag, if any.
func LogTagFrom(ctx context.Context) (LogTag, bool) {
	t, ok := ctx.Value(logTagKey{}).(LogTag)
	return t, ok && t.BookingID != ""
}

// IDSender is a Mailer that also reports the provider's id for the message, which is
// what a later bounce notification refers to. Resend does; SMTP has no such id.
type IDSender interface {
	SendID(ctx context.Context, msg Message) (string, error)
}

// SendID sends msg through m, returning the provider message id when m can report one.
func SendID(ctx context.Context, m Mailer, msg Message) (string, error) {
	if s, ok := m.(IDSender); ok {
		return s.SendID(ctx, msg)
	}
	return "", m.Send(ctx, msg)
}

// attendeeTag tags an attendee-facing email: the booker's own copy, or a guest's.
func (d BookingData) attendeeTag(ctx context.Context, kind string) context.Context {
	role := "attendee"
	if d.IsGuest {
		role = "guest"
	}
	return WithLogTag(ctx, LogTag{BookingID: d.BookingID, Kind: kind, Role: role})
}

// hostTag tags a host-facing email.
func (d BookingData) hostTag(ctx context.Context, kind string) context.Context {
	return WithLogTag(ctx, LogTag{BookingID: d.BookingID, Kind: kind, Role: "host"})
}
