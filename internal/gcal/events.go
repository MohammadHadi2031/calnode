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

// CreateEvent creates a Google Calendar event and returns its event ID and, when
// p.AddMeet is set, the generated Google Meet URL. Returns ("", "", nil) if the
// user has no is_destination connection.
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

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", "", fmt.Errorf("gcal: create event marshal: %w", err)
	}

	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events?sendUpdates=all"
	if p.AddMeet {
		apiURL += "&conferenceDataVersion=1" // required for conferenceData.createRequest to take effect
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", "", "", fmt.Errorf("gcal: create event request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("gcal: create event call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("gcal: create event status %d", resp.StatusCode)
	}

	var evResp calEventResp
	if err := json.NewDecoder(resp.Body).Decode(&evResp); err != nil {
		return "", "", "", fmt.Errorf("gcal: create event decode: %w", err)
	}
	return evResp.ID, evResp.meetLink(), calID, nil
}

// UpdateEvent moves an existing event to a new start/end (used on reschedule).
// Returns nil if eventID is empty or the user has no connection. sendUpdates=all
// so the attendee is notified of the new time.
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

	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events/" + url.PathEscape(eventID) + "?sendUpdates=all"
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

// AddAttendees adds people to an existing event and has Google invite them
// (sendUpdates=all). The event's current attendees are read first and sent back with the
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
	req, err = http.NewRequestWithContext(ctx, http.MethodPatch, eventURL+"?sendUpdates=all", bytes.NewReader(body))
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

// CancelEvent deletes a Google Calendar event by its event ID.
// Returns nil if eventID is empty or the user has no connection.
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

	apiURL := c.apiBase + "/calendars/" + url.PathEscape(calID) + "/events/" + url.PathEscape(eventID) + "?sendUpdates=all"
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
