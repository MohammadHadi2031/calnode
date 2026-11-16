package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/calnode/calnode/internal/calendar"
	"github.com/calnode/calnode/internal/calendar/microsoft"
)

// workspaceProvider stands in for microsoft.Workspace: a recording provider with a
// configured mailbox.
type workspaceProvider struct {
	*recordingProvider
	mailbox string
}

func (p *workspaceProvider) Mailbox(context.Context) (string, error) { return p.mailbox, nil }

// newWorkspaceFixture: both hosts book into Google only (nobody has Microsoft), and the
// workspace Teams account is configured. The event type is Teams, delivered as delivery.
func newWorkspaceFixture(t *testing.T, delivery string) (*meetFixture, *workspaceProvider) {
	t.Helper()
	f := newMeetFixture(t, "teams")
	if _, err := f.h.db.Exec(`UPDATE calendar_connections SET provider = 'google', account_kind = '' WHERE id = 'conn-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.db.Exec(`UPDATE event_types SET invite_delivery = ? WHERE slug = 'intro'`, delivery); err != nil {
		t.Fatal(err)
	}
	ws := &workspaceProvider{
		recordingProvider: &recordingProvider{inviteProvider: &inviteProvider{}, name: microsoft.WorkspaceName, link: "https://teams.microsoft.com/l/meetup-join/ws"},
		mailbox:           "appts@contoso.example",
	}
	f.h.getCal().SetWorkspace(ws)
	return f, ws
}

func emails(in []calendar.Invitee) string {
	var out []string
	for _, g := range in {
		out = append(out, strings.ToLower(g.Email))
	}
	return strings.Join(out, ",")
}

// The shared-account model: no host has Microsoft, yet a Teams booking gets its link from
// the workspace account, on one event that invites the booker and BOTH hosts (it is on
// neither host's calendar), stamped so reschedule and cancel find it again.
func TestWorkspaceTeams_mintsForHostsWithoutMicrosoft(t *testing.T) {
	f, ws := newWorkspaceFixture(t, "calendar")
	code, id := f.bookWith(t, "")
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	f.waitFor(t, inviteBookerEmail, 1)

	ww, gw, mw := ws.recorded(), f.google.recorded(), f.microsoft.recorded()
	if len(ww) != 1 || len(gw) != 0 || len(mw) != 0 {
		t.Fatalf("writes: workspace=%d google=%d microsoft=%d; want one workspace event", len(ww), len(gw), len(mw))
	}
	w := ww[0]
	if !w.params.AddMeet || w.params.OrganizerEmail != inviteBookerEmail {
		t.Errorf("AddMeet=%v booker=%q", w.params.AddMeet, w.params.OrganizerEmail)
	}
	if got, want := emails(w.params.Guests), inviteHostEmail+",second@gmail.example"; got != want {
		t.Errorf("guests = %s; want both hosts %s", got, want)
	}
	var loc, prov string
	if err := f.h.db.QueryRow(`SELECT location_value FROM bookings WHERE id = ?`, id).Scan(&loc); err != nil {
		t.Fatal(err)
	}
	if err := f.h.db.QueryRow(`SELECT COALESCE(external_provider,'') FROM booking_hosts
		WHERE booking_id = ? AND COALESCE(external_event_id,'') != ''`, id).Scan(&prov); err != nil {
		t.Fatal(err)
	}
	if loc != ws.link || prov != microsoft.WorkspaceName {
		t.Errorf("location = %q, provider = %q; want the workspace Teams link, stamped %s", loc, prov, microsoft.WorkspaceName)
	}
}

// Calnode-delivered invites keep the booker off provider events, so the workspace event
// would otherwise list nobody: the meeting host must be on it, or the Teams meeting they
// are hosting never reaches their own calendar.
func TestWorkspaceTeams_calnodeDeliveryInvitesTheHost(t *testing.T) {
	f, ws := newWorkspaceFixture(t, "calnode")
	if _, err := f.h.db.Exec(`DELETE FROM event_type_hosts WHERE user_id = 'host-2'`); err != nil {
		t.Fatal(err)
	}
	code, id := f.bookWith(t, "")
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	f.waitFor(t, inviteBookerEmail, 1)
	ww := ws.recorded()
	if len(ww) != 1 {
		t.Fatalf("workspace writes = %d; want 1", len(ww))
	}
	if ww[0].params.OrganizerEmail != "" {
		t.Errorf("booker %q on a Calnode-delivered event", ww[0].params.OrganizerEmail)
	}
	if got := emails(ww[0].params.Guests); got != inviteHostEmail {
		t.Errorf("guests = %q; want the host %s", got, inviteHostEmail)
	}
}

// Without a mailbox the workspace is off: the booking falls back to the manual link
// path and nothing is written to the workspace.
func TestWorkspaceTeams_unconfiguredIsOff(t *testing.T) {
	f, ws := newWorkspaceFixture(t, "calendar")
	ws.mailbox = ""
	code, id := f.bookWith(t, "")
	if code != 201 {
		t.Fatalf("create booking: %d — %s", code, id)
	}
	f.waitFor(t, inviteBookerEmail, 1)
	if n := len(ws.recorded()); n != 0 {
		t.Errorf("workspace writes = %d; want 0", n)
	}
}

func TestNormalizeTeamsMailbox(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		" Appts@Contoso.onmicrosoft.com ": "appts@contoso.onmicrosoft.com",
	} {
		if got, ok := normalizeTeamsMailbox(in); !ok || got != want {
			t.Errorf("normalizeTeamsMailbox(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"not-an-address", `"Appts" <appts@contoso.test>`} {
		if _, ok := normalizeTeamsMailbox(bad); ok {
			t.Errorf("normalizeTeamsMailbox(%q) accepted", bad)
		}
	}
}
