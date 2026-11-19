package handler

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/slots"
)

// flakyFreeBusy answers each call from script in order (nil = success); calls past the
// end succeed. hangFirst makes the first call block until its context ends, the way the
// production token refresh did.
type flakyFreeBusy struct {
	calendar.Provider
	script    []error
	hangFirst bool

	mu   sync.Mutex
	ctxs []context.Context
}

var busyBlock = []slots.Interval{{Start: time.Date(2026, 6, 15, 9, 0, 0, 0, time.UTC), End: time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)}}

func (p *flakyFreeBusy) Name() string { return "google" }

func (p *flakyFreeBusy) FreeBusy(ctx context.Context, _ string, _, _ time.Time) ([]slots.Interval, error) {
	p.mu.Lock()
	n := len(p.ctxs)
	p.ctxs = append(p.ctxs, ctx)
	p.mu.Unlock()
	if n == 0 && p.hangFirst {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if n < len(p.script) && p.script[n] != nil {
		return nil, p.script[n]
	}
	return busyBlock, nil
}

func (p *flakyFreeBusy) calls() []context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]context.Context(nil), p.ctxs...)
}

func quietHandler() *Handler { return &Handler{logger: slog.New(slog.DiscardHandler)} }

var errStalled = errors.New(`Post "https://oauth2.googleapis.com/token": context deadline exceeded`)

func TestFreeBusyRetried(t *testing.T) {
	invalidGrant := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	for _, tc := range []struct {
		name      string
		script    []error
		wantCalls int
		wantErr   error
	}{
		{"succeeds_first_time", nil, 1, nil},
		{"transient_failure_retried", []error{errStalled}, 2, nil},
		{"fails_twice", []error{errStalled, errStalled}, 2, errStalled},
		{"revoked_grant_not_retried", []error{invalidGrant}, 1, invalidGrant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &flakyFreeBusy{script: tc.script}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			busy, err := quietHandler().freeBusyRetried(ctx, p, "host", time.Time{}, time.Time{})
			calls := p.calls()
			if len(calls) != tc.wantCalls {
				t.Fatalf("FreeBusy called %d times, want %d", len(calls), tc.wantCalls)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if err == nil && len(busy) != 1 {
				t.Fatalf("busy = %v, want the provider's block", busy)
			}
			if calls[0].Value(oauth2.HTTPClient) != nil {
				t.Error("first try carries its own HTTP client; it should use the shared one")
			}
			if len(calls) == 2 {
				hc, _ := calls[1].Value(oauth2.HTTPClient).(interface{ CloseIdleConnections() })
				if hc == nil {
					t.Error("retry does not carry a fresh HTTP client, so it may reuse the stalled connection")
				}
			}
		})
	}
}

// The production failure: the first request hangs. The first try must give up at half
// the budget so the retry has time left, and the whole thing stays inside the deadline.
func TestFreeBusyRetried_hungFirstTryLeavesTimeForRetry(t *testing.T) {
	const budget = 400 * time.Millisecond
	p := &flakyFreeBusy{hangFirst: true}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	began := time.Now()
	busy, err := quietHandler().freeBusyRetried(ctx, p, "host", time.Time{}, time.Time{})
	elapsed := time.Since(began)
	if err != nil || len(busy) != 1 {
		t.Fatalf("busy = %v, err = %v; want the retry's answer", busy, err)
	}
	if len(p.calls()) != 2 {
		t.Fatalf("FreeBusy called %d times, want 2", len(p.calls()))
	}
	if elapsed < budget/2-20*time.Millisecond || elapsed > budget*3/4 {
		t.Errorf("took %v; want the first try cut off at about half of %v, then a fast retry", elapsed, budget)
	}
}

// Once the caller's own deadline has passed there is nothing to retry into.
func TestFreeBusyRetried_noRetryAfterCallerDeadline(t *testing.T) {
	p := &flakyFreeBusy{script: []error{errStalled}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := quietHandler().freeBusyRetried(ctx, p, "host", time.Time{}, time.Time{}); err == nil {
		t.Fatal("want the first try's error")
	}
	if len(p.calls()) != 1 {
		t.Fatalf("FreeBusy called %d times after the caller's deadline, want 1", len(p.calls()))
	}
}

// The Schedule page's warning says which kind of failure it was: one needs a retry, the
// other the host reconnecting their calendar.
func TestStaffConflicts_calendarUncheckedSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unreachable", errStalled, "try again in a moment"},
		{"revoked", &oauth2.RetrieveError{ErrorCode: "invalid_grant"}, "reconnect it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStaffFixture(t)
			svc := calendar.NewService(f.h.db)
			p := &flakyFreeBusy{script: []error{tc.err, tc.err}}
			svc.Register(p)
			f.h.SetCalendar(svc)
			start := f.day.Add(10 * time.Hour)
			cs, err := f.h.staffConflicts(context.Background(), f.etID, []string{f.owner.ID}, start, start.Add(30*time.Minute), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(cs) != 1 || cs[0].Kind != "calendar_unchecked" || !strings.Contains(cs[0].Detail, tc.want) {
				t.Fatalf("conflicts = %+v; want one calendar_unchecked saying %q", cs, tc.want)
			}
		})
	}
}

// End to end on the staff path that failed in production: a co-host's calendar that
// stalls once no longer blocks the booking.
func TestStaffConflicts_transientCalendarFailureRecovers(t *testing.T) {
	f := newStaffFixture(t)
	svc := calendar.NewService(f.h.db)
	p := &flakyFreeBusy{script: []error{errStalled}}
	svc.Register(p)
	f.h.SetCalendar(svc)
	start := f.day.Add(14 * time.Hour) // clear of busyBlock's date
	cs, err := f.h.staffConflicts(context.Background(), f.etID, []string{f.owner.ID}, start, start.Add(30*time.Minute), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatalf("conflicts = %+v; want none after the retry", cs)
	}
	if len(p.calls()) != 2 {
		t.Fatalf("FreeBusy called %d times, want 2", len(p.calls()))
	}
}
