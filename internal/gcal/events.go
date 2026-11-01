package gcal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/uid"
)

type calEventDateTime struct {
	DateTime string `json:"dateTime"`
	// omitempty: an update sends an absolute (offset-carrying) dateTime only, so the zone
	// the event was created in - the one its invitations render in - is left as it was.
	TimeZone string `json:"timeZone,omitempty"`
}

type calEventAttendee struct {
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
}

// conferenceData create-request: asks Google to allocate a Meet link for the event.
type calConferenceSolutionKey struct {
	Type string `json:"type"`
}
type calCreateConferenceRequest struct {
	RequestID             string                   `json:"requestId"`
	ConferenceSolutionKey calConferenceSolutionKey `json:"conferenceSolutionKey"`
}
type calConferenceData struct {
	CreateRequest *calCreateConferenceRequest `json:"createRequest,omitempty"`
}

type calEventReq struct {
	ICalUID        string             `json:"iCalUID,omitempty"`
	Sequence       int                `json:"sequence,omitempty"`
	Summary        string             `json:"summary"`
	Description    string             `json:"description,omitempty"`
	Location       string             `json:"location,omitempty"`
	Start          calEventDateTime   `json:"start"`
	End            calEventDateTime   `json:"end"`
	Attendees      []calEventAttendee `json:"attendees"`
	ConferenceData *calConferenceData `json:"conferenceData,omitempty"`
}

type calEntryPoint struct {
	EntryPointType string `json:"entryPointType"`
	URI            string `json:"uri"`
}

type calEventResp struct {
	ID             string `json:"id"`
	HangoutLink    string `json:"hangoutLink"`
	ConferenceData *struct {
		EntryPoints []calEntryPoint `json:"entryPoints"`
	} `json:"conferenceData"`
}

// meetLink extracts the Google Meet URL from a created event, preferring the
// top-level hangoutLink and falling back to the video conference entry point.
func (r calEventResp) meetLink() string {
	if r.HangoutLink != "" {
		return r.HangoutLink
	}
	if r.ConferenceData != nil {
		for _, ep := range r.ConferenceData.EntryPoints {
			if ep.EntryPointType == "video" && ep.URI != "" {
				return ep.URI
			}
		}
	}
	return ""
}

// quietUpdates is every write's sendUpdates: Google emails no attendee, ever. Its
// invitation shows times in the organizer's calendar time zone, whatever the event's own
// zone, so Calnode sends the invites instead (see InvitesGuests and EventInvite).
const quietUpdates = "sendUpdates=none"

// CreateEvent creates a Google Calendar event and returns its event ID and, when
// p.AddMeet is set, the generated Google Meet URL. Returns ("", "", nil) if the
// user has no is_destination connection. The attendees are put on the event but not
// emailed (quietUpdates). A p.ICalUID that Google refuses is dropped and the create
// retried, so a reused identity can only cost the booker a second entry, never the event.
func (c *Client) CreateEvent(ctx context.Context, userID string, p calendar.CreateEventParams) (string, string, string, error) {
	hc, calID, err := c.writeClient(ctx, userID, p.UseAnyAccount)
	if err != nil || hc == nil {
		return "", "", "", err
	}

	attendees := []calEventAttendee{}
	seen := map[string]bool{}
	add := func(email, name string) {
		key := strings.ToLower(email)
		if email == "" || seen[key] {
			return
		}
		seen[key] = true
		attendees = append(attendees, calEventAttendee{Email: email, DisplayName: name})
	}
	add(p.OrganizerEmail, p.OrganizerName)
	for _, g := range p.Guests {
		add(g.Email, g.Name)
	}

	start, end := eventTimes(p.Start, p.End, p.TimeZone)
	reqBody := calEventReq{
		Summary:     p.Summary,
		Description: p.Description,
		Location:    p.Location,
		Start:       start,
		End:         end,
		Attendees:   attendees,
	}
	if p.AddMeet {
		reqBody.ConferenceData = &calConferenceData{
			CreateRequest: &calCreateConferenceRequest{
				RequestID:             uid.New(), // unique per request; Google dedupes conference creation on it
				ConferenceSolutionKey: calConferenceSolutionKey{Type: "hangoutsMeet"},
			},
		}
	}

	if p.ICalUID != "" {
		reqBody.ICalUID, reqBody.Sequence = p.ICalUID, p.Sequence
	}

	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events?" + quietUpdates
	if p.AddMeet {
		apiURL += "&conferenceDataVersion=1" // required for conferenceData.createRequest to take effect
	}
	evResp, status, err := insertEvent(ctx, hc, apiURL, reqBody)
	if err == nil && reqBody.ICalUID != "" && status >= 400 && status < 500 {
		reqBody.ICalUID, reqBody.Sequence = "", 0
		evResp, status, err = insertEvent(ctx, hc, apiURL, reqBody)
	}
	if err != nil {
		return "", "", "", err
	}
	if status != http.StatusOK {
		return "", "", "", fmt.Errorf("gcal: create event status %d", status)
	}
	return evResp.ID, evResp.meetLink(), calID, nil
}

// insertEvent POSTs one events.insert and decodes a 200 reply. Any other status is
// returned rather than turned into an error, so the caller can decide to retry.
func insertEvent(ctx context.Context, hc *http.Client, apiURL string, reqBody calEventReq) (calEventResp, int, error) {
	var evResp calEventResp
	body, err := json.Marshal(reqBody)
	if err != nil {
		return evResp, 0, fmt.Errorf("gcal: create event marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return evResp, 0, fmt.Errorf("gcal: create event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return evResp, 0, fmt.Errorf("gcal: create event call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return evResp, resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(&evResp); err != nil {
		return evResp, 0, fmt.Errorf("gcal: create event decode: %w", err)
	}
	return evResp, http.StatusOK, nil
}

// EventInvite reads an event's iCalendar identity - UID, SEQUENCE and organizer - for the
// .ics Calnode sends in Google's place (calendar.QuietInviter).
func (c *Client) EventInvite(ctx context.Context, userID, calendarID, eventID string) (calendar.EventInvite, error) {
	var inv calendar.EventInvite
	if eventID == "" {
		return inv, fmt.Errorf("gcal: event invite: no event id")
	}
	hc, calID, err := c.writeClient(ctx, userID, true)
	if err != nil {
		return inv, err
	}
	if hc == nil {
		return inv, fmt.Errorf("gcal: event invite: no connected account")
	}
	if calendarID != "" {
		calID = calendarID
	}
	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events/" + url.PathEscape(eventID) +
		"?fields=" + url.QueryEscape("iCalUID,sequence,organizer(email,displayName)")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return inv, fmt.Errorf("gcal: event invite request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return inv, fmt.Errorf("gcal: event invite call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return inv, fmt.Errorf("gcal: event invite status %d", resp.StatusCode)
	}
	var ev struct {
		ICalUID   string `json:"iCalUID"`
		Sequence  int    `json:"sequence"`
		Organizer struct {
			Email       string `json:"email"`
			DisplayName string `json:"displayName"`
		} `json:"organizer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return inv, fmt.Errorf("gcal: event invite decode: %w", err)
	}
	if ev.ICalUID == "" {
		return inv, fmt.Errorf("gcal: event invite: no iCalUID")
	}
	return calendar.EventInvite{
		UID:            ev.ICalUID,
		Sequence:       ev.Sequence,
		OrganizerName:  ev.Organizer.DisplayName,
		OrganizerEmail: ev.Organizer.Email,
	}, nil
}

// UpdateEvent moves an existing event to a new start/end (used on reschedule).
// Returns nil if eventID is empty or the user has no connection. Nobody is emailed
// (quietUpdates): Calnode sends the attendees the updated invite.
func (c *Client) UpdateEvent(ctx context.Context, userID, calendarID, eventID string, start, end time.Time) error {
	if eventID == "" {
		return nil
	}
	hc, calID, err := c.writeClient(ctx, userID, true)
	if err != nil || hc == nil {
		return err
	}
	// Act on the calendar the event was created in, not wherever the destination points
	// now. Empty means the booking predates that being recorded, so fall back.
	//
	// Limitation: this only rescues a change of calendar WITHIN the connected account. If
	// the host moves their destination to a different account entirely, hc is that other
	// account's client and the old event is out of reach - correcting that needs the
	// account recorded too, which is a bigger change than the bug currently justifies.
	if calendarID != "" {
		calID = calendarID
	}

	body, err := json.Marshal(struct {
		Start calEventDateTime `json:"start"`
		End   calEventDateTime `json:"end"`
	}{
		Start: calEventDateTime{DateTime: start.UTC().Format(time.RFC3339)},
		End:   calEventDateTime{DateTime: end.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return fmt.Errorf("gcal: update event marshal: %w", err)
	}

	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events/" + url.PathEscape(eventID) + "?" + quietUpdates
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, apiURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gcal: update event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gcal: update event call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gcal: update event status %d", resp.StatusCode)
	}
	return nil
}

// AddAttendees adds people to an existing event without Google emailing them
// (quietUpdates; Calnode invites them). The event's current attendees are read first and sent back with the
// new ones, since a PATCH replaces the whole list; their RSVP state is kept as is.
func (c *Client) AddAttendees(ctx context.Context, userID, calendarID, eventID string, add []calendar.Invitee) error {
	if eventID == "" || len(add) == 0 {
		return nil
	}
	hc, calID, err := c.writeClient(ctx, userID, true)
	if err != nil {
		return err
	}
	if hc == nil {
		return calendar.ErrCannotAddAttendees
	}
	if calendarID != "" {
		calID = calendarID
	}
	eventURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events/" + url.PathEscape(eventID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventURL+"?fields=attendees", nil)
	if err != nil {
		return fmt.Errorf("gcal: get attendees request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gcal: get attendees call: %w", err)
	}
	// Raw messages, so every attendee goes back exactly as Google returned it.
	var current struct {
		Attendees []json.RawMessage `json:"attendees"`
	}
	err = json.NewDecoder(resp.Body).Decode(&current)
	resp.Body.Close() // #nosec G104 -- read fully above; nothing actionable on close
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gcal: get attendees status %d", resp.StatusCode)
	}
	if err != nil {
		return fmt.Errorf("gcal: get attendees decode: %w", err)
	}

	seen := map[string]bool{}
	attendees := current.Attendees
	for _, a := range attendees {
		var e struct {
			Email string `json:"email"`
		}
		_ = json.Unmarshal(a, &e)
		seen[strings.ToLower(e.Email)] = true
	}
	added := false
	for _, g := range add {
		key := strings.ToLower(g.Email)
		if g.Email == "" || seen[key] {
			continue
		}
		seen[key] = true
		raw, _ := json.Marshal(calEventAttendee{Email: g.Email, DisplayName: g.Name})
		attendees = append(attendees, raw)
		added = true
	}
	if !added {
		return nil
	}

	body, err := json.Marshal(map[string]any{"attendees": attendees})
	if err != nil {
		return fmt.Errorf("gcal: add attendees marshal: %w", err)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodPatch, eventURL+"?"+quietUpdates, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("gcal: add attendees request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = hc.Do(req)
	if err != nil {
		return fmt.Errorf("gcal: add attendees call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gcal: add attendees status %d", resp.StatusCode)
	}
	return nil
}

// CancelEvent deletes a Google Calendar event by its event ID, emailing nobody
// (quietUpdates; Calnode sends the cancellation). Returns nil if eventID is empty or
// the user has no connection.
func (c *Client) CancelEvent(ctx context.Context, userID, calendarID, eventID string) error {
	if eventID == "" {
		return nil
	}
	hc, calID, err := c.writeClient(ctx, userID, true)
	if err != nil || hc == nil {
		return err
	}
	// Act on the calendar the event was created in, not wherever the destination points
	// now. Empty means the booking predates that being recorded, so fall back.
	//
	// Limitation: this only rescues a change of calendar WITHIN the connected account. If
	// the host moves their destination to a different account entirely, hc is that other
	// account's client and the old event is out of reach - correcting that needs the
	// account recorded too, which is a bigger change than the bug currently justifies.
	if calendarID != "" {
		calID = calendarID
	}

	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events/" + url.PathEscape(eventID) + "?" + quietUpdates
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, apiURL, nil)
	if err != nil {
		return fmt.Errorf("gcal: cancel event request: %w", err)
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("gcal: cancel event call: %w", err)
	}
	defer resp.Body.Close()

	// 204 No Content = deleted; 410 Gone = already deleted — both are fine.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusGone {
		return fmt.Errorf("gcal: cancel event status %d", resp.StatusCode)
	}
	return nil
}

// eventTimes writes start/end as wall-clock times in the IANA zone tz, falling back to
// UTC for "" or a zone this build does not know.
func eventTimes(start, end time.Time, tz string) (calEventDateTime, calEventDateTime) {
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	name := loc.String()
	return calEventDateTime{DateTime: start.In(loc).Format(time.RFC3339), TimeZone: name},
		calEventDateTime{DateTime: end.In(loc).Format(time.RFC3339), TimeZone: name}
}
