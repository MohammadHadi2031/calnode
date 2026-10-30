package mailer

import (
	"context"
	"sync"
)

// Live is a Mailer that wraps a hot-swappable inner Mailer. All calls to Send
// are forwarded to whatever the current inner Mailer is at the time of the call,
// so SMTP settings can be changed at runtime without restarting the server.
type Live struct {
	mu       sync.RWMutex
	current  Mailer
	observer SendObserver
}

// SendObserver watches booking emails (sends carrying a LogTag) going through a Live
// mailer: Begin runs before the send, and the func it returns runs with the outcome.
// It is how the email log records every attempt without wrapping Live, which callers
// type-assert for Swap and IsEnabled.
type SendObserver interface {
	Begin(ctx context.Context, tag LogTag, msg Message) (done func(providerID string, err error))
}

// NewLive creates a Live mailer with the given initial Mailer.
func NewLive(initial Mailer) *Live {
	return &Live{current: initial}
}

// Send forwards to the current inner Mailer.
func (l *Live) Send(ctx context.Context, msg Message) error {
	_, err := l.SendID(ctx, msg)
	return err
}

// SendID forwards to the current inner Mailer, reporting its message id when it has one.
func (l *Live) SendID(ctx context.Context, msg Message) (string, error) {
	l.mu.RLock()
	m, obs := l.current, l.observer
	l.mu.RUnlock()
	tag, tagged := LogTagFrom(ctx)
	if obs == nil || !tagged {
		return SendID(ctx, m, msg)
	}
	done := obs.Begin(ctx, tag, msg)
	if _, noop := m.(*Noop); noop {
		// Nothing was delivered, whatever Noop reports. Log it as such, but keep returning
		// nil: an unconfigured mailer has never been an error for the booking flow.
		done("", ErrNotConfigured)
		return "", nil
	}
	id, err := SendID(ctx, m, msg)
	done(id, err)
	return id, err
}

// Swap atomically replaces the inner Mailer.
func (l *Live) Swap(m Mailer) {
	l.mu.Lock()
	l.current = m
	l.mu.Unlock()
}

// SetObserver installs o to watch every tagged send. nil removes it.
func (l *Live) SetObserver(o SendObserver) {
	l.mu.Lock()
	l.observer = o
	l.mu.Unlock()
}

// IsEnabled reports whether the current inner Mailer is a real sender (not Noop).
func (l *Live) IsEnabled() bool {
	l.mu.RLock()
	_, noop := l.current.(*Noop)
	l.mu.RUnlock()
	return !noop
}
