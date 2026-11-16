package microsoft

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"golang.org/x/oauth2/microsoft"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/slots"
)

// WorkspaceName is the provider name stamped on bookings whose Teams meeting the
// workspace account organized (booking_hosts.external_provider). It routes their later
// updates and cancels back here rather than to any host's own Microsoft connection.
const WorkspaceName = "microsoft_workspace"

// Workspace organizes Teams meetings from ONE Microsoft 365 mailbox for every host, using
// the app's own identity (OAuth client credentials) instead of anyone's sign-in.
//
// Why app-only rather than a stored user token: the mailbox is a shared service account.
// A delegated token for it breaks when its password changes, when MFA or conditional
// access re-prompts, or after 90 days unused, and getting one means somebody signs in as
// it. Client credentials only stop working when the app's client secret expires, which
// already stops every other Microsoft feature, so it adds no new way to fail.
//
// The mailbox is read from server_settings on every call, so the owner can change it in
// Settings without a restart. Each event records the mailbox it was written to (as its
// calendar id), so changing the setting later never strands the meetings already booked.
//
// It is deliberately never a destination and never consulted for free/busy: a calendar
// every host shares would make each host's meetings look like everyone's busy time.
type Workspace struct {
	db      *sql.DB
	hc      *http.Client
	tokens  *refreshableSource // nil in tests that inject hc directly
	apiBase string
}

// refreshableSource is a cached client-credentials token source that can be told to drop
// its token. The cache matters: a token is good for about an hour, and fetching one per
// Graph call would double every request. But a token's permissions are fixed when it is
// issued, so one fetched before an admin granted Calendars.ReadWrite keeps failing for
// that whole hour - which is exactly when the admin presses Test. Check refreshes it.
type refreshableSource struct {
	mu  sync.Mutex
	ctx context.Context
	cfg *clientcredentials.Config
	src oauth2.TokenSource
}

func newRefreshableSource(ctx context.Context, cfg *clientcredentials.Config) *refreshableSource {
	return &refreshableSource{ctx: ctx, cfg: cfg, src: cfg.TokenSource(ctx)}
}

// tokenClient is an HTTP client authorized by tokens. Not oauth2.NewClient: that wraps the
// source in a cache of its own, which would hold on to the token Refresh just dropped.
func tokenClient(tokens *refreshableSource, base http.RoundTripper) *http.Client {
	return &http.Client{Transport: &oauth2.Transport{Source: tokens, Base: base}}
}

// Token returns the cached token, fetching one when it is missing or expired.
func (r *refreshableSource) Token() (*oauth2.Token, error) {
	r.mu.Lock()
	src := r.src
	r.mu.Unlock()
	return src.Token()
}

// Refresh drops the cached token, so the next call fetches one with current permissions.
func (r *refreshableSource) Refresh() {
	r.mu.Lock()
	r.src = r.cfg.TokenSource(r.ctx)
	r.mu.Unlock()
}

var (
	_ calendar.Provider          = (*Workspace)(nil)
	_ calendar.AttendeeAdder     = (*Workspace)(nil)
	_ calendar.WorkspaceMeetings = (*Workspace)(nil)
)

// ErrWorkspaceTenant means MICROSOFT_TENANT does not name one organization. Client
// credentials are issued by a specific tenant; "common" and its siblings are sign-in
// routing aliases that cannot issue them.
var ErrWorkspaceTenant = errors.New("MICROSOFT_TENANT must name your organization (its …onmicrosoft.com domain or tenant ID), not common/organizations/consumers")

// errWorkspaceNoOAuth is returned by the OAuth half of calendar.Provider, which nobody
// should reach: the workspace account is configured in Settings, never connected.
var errWorkspaceNoOAuth = errors.New("microsoft workspace: configured in Settings, not connected through OAuth")

// NewWorkspace returns the workspace Teams organizer for the app registration clientID in
// tenant. It makes no network call: a wrong secret or missing permission shows up on the
// first booking or, earlier, in Settings → Microsoft Teams → Test.
func NewWorkspace(db *sql.DB, clientID, clientSecret, tenant string) (*Workspace, error) {
	switch strings.ToLower(strings.TrimSpace(tenant)) {
	case "", "common", "organizations", "consumers":
		return nil, ErrWorkspaceTenant
	}
	cc := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     microsoft.AzureADEndpoint(tenant).TokenURL,
		Scopes:       []string{"https://graph.microsoft.com/.default"},
	}
	// Bounded on both legs: the token fetch (the context's client) and the Graph calls.
	base := &http.Client{Timeout: 20 * time.Second}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, base)
	tokens := newRefreshableSource(ctx, cc)
	hc := tokenClient(tokens, base.Transport)
	hc.Timeout = 30 * time.Second
	return &Workspace{db: db, hc: hc, tokens: tokens, apiBase: "https://graph.microsoft.com/v1.0"}, nil
}

// Mailbox is the configured workspace mailbox, "" when the owner has not set one.
func (w *Workspace) Mailbox(ctx context.Context) (string, error) {
	var mb string
	err := w.db.QueryRowContext(ctx, `SELECT COALESCE(teams_mailbox,'') FROM server_settings WHERE id = 1`).Scan(&mb)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return strings.TrimSpace(mb), err
}

// mailboxFor is the mailbox an existing event lives in: the one recorded at creation
// (calendarID), else the current setting for an event written without one.
func (w *Workspace) mailboxFor(ctx context.Context, calendarID string) (string, error) {
	if calendarID != "" {
		return calendarID, nil
	}
	return w.Mailbox(ctx)
}

func (w *Workspace) mailboxRoot(mailbox string) string {
	return w.apiBase + "/users/" + url.PathEscape(mailbox)
}

func (w *Workspace) eventURL(mailbox, eventID string) string {
	return w.mailboxRoot(mailbox) + "/events/" + url.PathEscape(eventID)
}

// Name identifies this provider on the bookings it writes.
func (w *Workspace) Name() string { return WorkspaceName }

// InvitesGuests is true: Graph emails the event's attendees itself.
func (w *Workspace) InvitesGuests() bool { return true }

// AuthURL is empty: there is no consent flow for the workspace account.
func (w *Workspace) AuthURL(string) string { return "" }

// EncryptState is not supported (see errWorkspaceNoOAuth).
func (w *Workspace) EncryptState(string) (string, error) { return "", errWorkspaceNoOAuth }

// DecryptState is not supported (see errWorkspaceNoOAuth).
func (w *Workspace) DecryptState(string) (string, error) { return "", errWorkspaceNoOAuth }

// Exchange is not supported (see errWorkspaceNoOAuth).
func (w *Workspace) Exchange(context.Context, string, string, string) error {
	return errWorkspaceNoOAuth
}

// Connected is false for every user: nobody connects the workspace account.
func (w *Workspace) Connected(context.Context, string) (bool, error) { return false, nil }

// Disconnect is a no-op; the owner clears the mailbox in Settings instead.
func (w *Workspace) Disconnect(context.Context, string) error { return nil }

// HasDestination is false: bookings are never written here as a host's own calendar.
func (w *Workspace) HasDestination(context.Context, string) (bool, error) { return false, nil }

// ListCalendars lists nothing: there is no per-host calendar picker for this account.
func (w *Workspace) ListCalendars(context.Context, string, string) ([]calendar.CalendarInfo, error) {
	return nil, nil
}

// FreeBusy is always empty. See the type comment for why this account is never checked.
func (w *Workspace) FreeBusy(context.Context, string, time.Time, time.Time) ([]slots.Interval, error) {
	return nil, nil
}

// CreateEvent writes the event to the workspace mailbox's default calendar, with a Teams
// meeting when p.AddMeet is set. userID is ignored: the mailbox is the organizer whoever
// the host is, so the caller must invite the host (see sharedEventInvitees). The returned
// calendar id is the mailbox, so later calls reach this event even if the setting changes.
// ("", "", "", nil) when no mailbox is configured.
func (w *Workspace) CreateEvent(ctx context.Context, _ string, p calendar.CreateEventParams) (string, string, string, error) {
	mb, err := w.Mailbox(ctx)
	if err != nil || mb == "" {
		return "", "", "", err
	}
	eventID, join, err := createGraphEvent(ctx, w.hc, w.mailboxRoot(mb)+"/events", p)
	if err != nil {
		return "", "", "", err
	}
	return eventID, join, mb, nil
}

// UpdateEvent moves the event in the mailbox recorded at creation (calendarID).
func (w *Workspace) UpdateEvent(ctx context.Context, _, calendarID, eventID string, start, end time.Time) error {
	if eventID == "" {
		return nil
	}
	mb, err := w.mailboxFor(ctx, calendarID)
	if err != nil || mb == "" {
		return err
	}
	return moveGraphEvent(ctx, w.hc, w.eventURL(mb, eventID), start, end)
}

// AddAttendees adds people to the event in the mailbox recorded at creation.
func (w *Workspace) AddAttendees(ctx context.Context, _, calendarID, eventID string, add []calendar.Invitee) error {
	if eventID == "" || len(add) == 0 {
		return nil
	}
	mb, err := w.mailboxFor(ctx, calendarID)
	if err != nil {
		return err
	}
	if mb == "" {
		return calendar.ErrCannotAddAttendees
	}
	return patchGraphAttendees(ctx, w.hc, w.eventURL(mb, eventID), nil, add)
}

// ReplaceAttendee swaps one attendee for another on an existing event (a reassigned
// host). The event stays where it is, so the Teams meeting, its join link and the
// booker's invite are all untouched: only the departing host gets a cancellation and the
// new one an invite. Deleting and re-creating the event, as reassignment does for a
// host's own calendar, would cancel the meeting the booker already holds.
func (w *Workspace) ReplaceAttendee(ctx context.Context, calendarID, eventID, removeEmail string, add calendar.Invitee) error {
	if eventID == "" {
		return nil
	}
	mb, err := w.mailboxFor(ctx, calendarID)
	if err != nil {
		return err
	}
	if mb == "" {
		return calendar.ErrCannotAddAttendees
	}
	var remove []string
	if removeEmail != "" && !strings.EqualFold(removeEmail, add.Email) {
		remove = []string{removeEmail}
	}
	return patchGraphAttendees(ctx, w.hc, w.eventURL(mb, eventID), remove, []calendar.Invitee{add})
}

// CancelEvent deletes the event in the mailbox recorded at creation.
func (w *Workspace) CancelEvent(ctx context.Context, _, calendarID, eventID string) error {
	if eventID == "" {
		return nil
	}
	mb, err := w.mailboxFor(ctx, calendarID)
	if err != nil || mb == "" {
		return err
	}
	return deleteGraphEvent(ctx, w.hc, w.eventURL(mb, eventID))
}

// WorkspaceCheck is the result of Check, phrased for the Settings page.
type WorkspaceCheck struct {
	// OK: the app can reach the mailbox's calendar AND it may create Teams meetings.
	OK bool `json:"ok"`
	// Reachable: the token was issued and the mailbox's calendar was read.
	Reachable bool `json:"reachable"`
	// Providers is the calendar's allowedOnlineMeetingProviders as Graph reported it.
	Providers []string `json:"providers"`
	// Message says what is wrong, and where to fix it, when OK is false.
	Message string `json:"message,omitempty"`
}

// Check verifies mailbox end to end without creating anything: that Microsoft issues the
// app a token, that the app may read the mailbox's calendar, and that the calendar lists
// teamsForBusiness among its allowed meeting providers. The last part is exactly what
// fails when Graph accepts an event but silently drops its Teams meeting.
//
// It always starts from a fresh token (see refreshableSource): Test is what an admin
// presses right after changing permissions, and bookings then use that token too.
func (w *Workspace) Check(ctx context.Context, mailbox string) WorkspaceCheck {
	if w.tokens != nil {
		w.tokens.Refresh()
	}
	u := w.mailboxRoot(mailbox) + "/calendar?$select=allowedOnlineMeetingProviders"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return WorkspaceCheck{Message: err.Error()}
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			return WorkspaceCheck{Message: "Microsoft refused the app's credentials (check MICROSOFT_CLIENT_SECRET and MICROSOFT_TENANT): " + tokenErrSummary(re)}
		}
		return WorkspaceCheck{Message: "Could not reach Microsoft: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return WorkspaceCheck{Message: graphCheckMessage(resp.StatusCode, graphErrBody(resp))}
	}
	var cal struct {
		Allowed []string `json:"allowedOnlineMeetingProviders"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cal); err != nil {
		return WorkspaceCheck{Message: "Unexpected reply from Microsoft: " + err.Error()}
	}
	out := WorkspaceCheck{Reachable: true, Providers: cal.Allowed}
	for _, p := range cal.Allowed {
		if strings.EqualFold(p, "teamsForBusiness") {
			out.OK = true
			return out
		}
	}
	out.Message = "The mailbox is reachable, but Microsoft does not allow it to create Teams meetings yet " +
		"(teamsForBusiness is not among its allowed meeting providers). Check that the account has a " +
		"Teams license and Teams-only upgrade mode; a newly licensed account can take up to a day."
	return out
}

// graphCheckMessage turns a failed calendar read into the step that fixes it.
func graphCheckMessage(status int, body string) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &e)
	code := e.Error.Code
	switch {
	case code == "MailboxNotEnabledForRESTAPI" || code == "MailboxNotHostedInExchangeOnline":
		return "That account has no Exchange Online mailbox. Its license must include Exchange Online."
	case status == http.StatusNotFound || code == "ErrorInvalidUser" || code == "ResourceNotFound":
		return "No mailbox with that address exists in your organization."
	case status == http.StatusUnauthorized || status == http.StatusForbidden || code == "ErrorAccessDenied":
		return "The app may not use this mailbox. In Entra → App registrations → API permissions, add " +
			"Microsoft Graph → Application → Calendars.ReadWrite and grant admin consent. If you limited the " +
			"app to certain mailboxes, include this one. Changes can take up to an hour to apply."
	}
	if e.Error.Message != "" {
		return fmt.Sprintf("Microsoft returned %d: %s", status, e.Error.Message)
	}
	return fmt.Sprintf("Microsoft returned %d", status)
}

// tokenErrSummary is the first line of an AADSTS error, which names the actual cause
// (e.g. AADSTS7000215 invalid client secret) without the trace and correlation ids.
func tokenErrSummary(re *oauth2.RetrieveError) string {
	msg := re.ErrorDescription
	if msg == "" {
		msg = re.ErrorCode
	}
	if msg == "" {
		msg = re.Error()
	}
	if i := strings.IndexAny(msg, "\r\n"); i > 0 {
		msg = msg[:i]
	}
	if i := strings.Index(msg, " Trace ID:"); i > 0 {
		msg = msg[:i] // the trace/correlation ids help Microsoft support, not the admin
	}
	return msg
}
