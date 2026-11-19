package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/slots"
)

var errCalendarUnavailable = errors.New("calendar availability could not be checked; please try again shortly")

// calendarCheckTimeout bounds the whole booking-time free/busy check. Each host is
// checked concurrently, so this is roughly one provider round-trip (plus a token
// refresh), not one per host — the check used to run host by host under this same
// budget, which a collective with a few slow (often Microsoft Graph) calendars could
// exhaust, failing the booking with errCalendarUnavailable for no fault of any host.
var calendarCheckTimeout = 15 * time.Second

// calendarFreeHosts re-checks external calendar busy at booking time and returns the
// candidates and optional hosts who are actually free.
//
// Fail-closed only where a host's attendance is mandatory: an error on a required host,
// or on any fixed/collective candidate, is errCalendarUnavailable — booking a seat whose
// calendar could not be read risks a double-booking. Elsewhere an unreadable calendar
// just removes that host from consideration: a round-robin candidate is skipped (the
// rotation picks among the hosts verified free), and an optional host is left off the
// booking. One host's revoked token therefore no longer blocks every booking on a team.
func (h *Handler) calendarFreeHosts(ctx context.Context, et *bookableEventType, candidates, required, optional []string, start, end time.Time) ([]string, []string, error) {
	gc := h.getCal()
	if gc == nil {
		return candidates, optional, nil
	}
	ctx, cancel := context.WithTimeout(ctx, calendarCheckTimeout)
	defer cancel()
	window := slots.Interval{
		Start: start.Add(-time.Duration(et.BufferAfterMinutes) * time.Minute),
		End:   end.Add(time.Duration(et.BufferBeforeMinutes) * time.Minute),
	}

	type verdict struct {
		free bool
		err  error
	}
	var ids []string
	for _, group := range [][]string{required, candidates, optional} {
		for _, id := range group {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	verdicts := make(map[string]verdict, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			free, err := h.calendarFreeAt(ctx, gc, id, window)
			mu.Lock()
			verdicts[id] = verdict{free, err}
			mu.Unlock()
		}(id)
	}
	wg.Wait()

	for _, id := range required {
		v := verdicts[id]
		if v.err != nil {
			return nil, nil, v.err
		}
		if !v.free {
			return nil, nil, errSlotUnavailable
		}
	}
	available := make([]string, 0, len(candidates))
	var candidateErr error
	for _, id := range candidates {
		v := verdicts[id]
		switch {
		case v.err != nil && et.RoutingMode != "round_robin":
			return nil, nil, v.err
		case v.err != nil:
			h.logger.WarnContext(ctx, "booking calendar check: skipping rotation host", "host", id, "error", v.err)
			candidateErr = v.err
		case v.free:
			available = append(available, id)
		case et.RoutingMode != "round_robin":
			return nil, nil, errSlotUnavailable
		}
	}
	if len(available) == 0 {
		if candidateErr != nil {
			return nil, nil, candidateErr // nobody verified free, and at least one unknown
		}
		return nil, nil, errSlotUnavailable
	}
	guests := make([]string, 0, len(optional))
	for _, id := range optional {
		v := verdicts[id]
		if v.err != nil {
			h.logger.WarnContext(ctx, "booking calendar check: leaving optional host off", "host", id, "error", v.err)
			continue
		}
		if v.free {
			guests = append(guests, id)
		}
	}
	return available, guests, nil
}

// calendarFreeAt reports whether userID's external calendars are free for the whole
// window, ignoring Calnode's own events (the DB is the source of truth for those).
func (h *Handler) calendarFreeAt(ctx context.Context, gc calendarFreeBusy, userID string, window slots.Interval) (bool, error) {
	ownEvents, err := h.ownCalendarEvents(ctx, userID, window.Start.Format(time.RFC3339Nano), window.End.Format(time.RFC3339Nano))
	if err != nil {
		if ctx.Err() != nil { // the check's deadline, not a DB fault: same "try again" as a slow provider
			return false, fmt.Errorf("%w: load own calendar events: %w", errCalendarUnavailable, err)
		}
		return false, fmt.Errorf("load own calendar events: %w", err)
	}
	busy, err := h.freeBusyRetried(ctx, gc, userID, window.Start, window.End)
	if err != nil {
		return false, fmt.Errorf("%w: %w", errCalendarUnavailable, err)
	}
	busy = slots.SubtractIntervals(busy, ownEvents)
	remaining := slots.SubtractIntervals([]slots.Interval{window}, busy)
	return len(remaining) == 1 && remaining[0] == window, nil
}

// freeBusyRetried is gc.FreeBusy with one retry, so a single stalled request to the
// provider does not refuse a booking. In production a Google token refresh hung until the
// whole check's deadline, blocking a staff booking on a co-host's calendar that was fine
// a moment later.
//
// The first try gets half of ctx's remaining time, leaving the retry the rest, so the
// worst case is still the caller's deadline. A revoked or expired grant is not retried:
// the remedy is reconnecting the account, and asking again gets the same answer.
//
// The retry runs on its own connection pool, passed the way golang.org/x/oauth2 reads it
// (the oauth2.HTTPClient context value, which the Google and Microsoft providers' token
// refresh and API calls both use). A cancelled HTTP/2 request leaves its connection in
// the shared pool, so without this the retry could be sent down the connection that
// just stalled.
func (h *Handler) freeBusyRetried(ctx context.Context, gc calendarFreeBusy, userID string, from, to time.Time) ([]slots.Interval, error) {
	first, cancel := ctx, context.CancelFunc(func() {})
	if deadline, ok := ctx.Deadline(); ok {
		first, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(deadline)/2))
	}
	busy, err := gc.FreeBusy(first, userID, from, to)
	cancel()
	if err == nil || ctx.Err() != nil || calendar.IsReauthErr(err) {
		return busy, err
	}
	h.logger.WarnContext(ctx, "calendar check: retrying free/busy", "host", userID, "error", err)
	retry := ctx
	if ctx.Value(oauth2.HTTPClient) == nil {
		var t *http.Transport
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			t = base.Clone()
		} else {
			t = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
		defer t.CloseIdleConnections()
		retry = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: t})
	}
	return gc.FreeBusy(retry, userID, from, to)
}

// calendarFreeBusy is the slice of *calendar.Service the booking-time check uses.
type calendarFreeBusy interface {
	FreeBusy(ctx context.Context, userID string, from, to time.Time) ([]slots.Interval, error)
}
