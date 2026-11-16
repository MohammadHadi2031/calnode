package handler

import (
	"context"
	"fmt"
	"strings"
)

// Conference options (wisegrid fork, migration 90006): the video platforms a booker may
// choose between. The event type's location_type is the default and is always offered;
// conference_options lists the platforms offered beside it. Only the platforms Calnode
// can mint a meeting for per booking are eligible, because a switch drops the event
// type's fixed link (that link is for the default platform), so an alternative that
// cannot be minted would leave the booking with no way to join.

// conferencePlatforms are the eligible platforms, in the order the booker sees them.
var conferencePlatforms = []string{"google_meet", "teams", "zoom"}

// conferencePlatform reports whether t is a platform a booker may choose.
func conferencePlatform(t string) bool {
	for _, p := range conferencePlatforms {
		if p == t {
			return true
		}
	}
	return false
}

// parseConferenceOptions reads the stored list, dropping blanks, unknown names and
// duplicates, in canonical order.
func parseConferenceOptions(stored string) []string {
	have := map[string]bool{}
	for _, p := range strings.Split(stored, ",") {
		have[strings.TrimSpace(p)] = true
	}
	out := []string{}
	for _, p := range conferencePlatforms {
		if have[p] {
			out = append(out, p)
		}
	}
	return out
}

// encodeConferenceOptions is parseConferenceOptions' inverse: canonical, deduplicated.
func encodeConferenceOptions(opts []string) string {
	return strings.Join(parseConferenceOptions(strings.Join(opts, ",")), ",")
}

// effectiveConferenceOptions is what the booker is actually offered: the stored list
// plus the default (locType), in canonical order - or nil when there is no choice to
// make, because locType is not a platform (a phone number, an address) or nothing else
// is offered beside it.
func effectiveConferenceOptions(stored, locType string) []string {
	if !conferencePlatform(locType) {
		return nil
	}
	opts := parseConferenceOptions(stored + "," + locType)
	if len(opts) < 2 {
		return nil
	}
	return opts
}

// offersConference reports whether choice is one of the platforms the booker is offered.
func offersConference(stored, locType, choice string) bool {
	for _, p := range effectiveConferenceOptions(stored, locType) {
		if p == choice {
			return true
		}
	}
	return false
}

// validateConferenceOptions checks that every platform the change ADDS (offered now and
// not before, other than the default, which validateLocation covers) can mint a meeting
// for this owner. Validated on change, not on mention: a platform already offered stays
// saveable after its account is disconnected, as CLAUDE.md requires for every field the
// editor re-submits. Returns a user-facing error.
func (h *Handler) validateConferenceOptions(ctx context.Context, ownerID, locType string, before, after []string) error {
	had := map[string]bool{locType: true}
	for _, p := range before {
		had[p] = true
	}
	for _, p := range after {
		if had[p] {
			continue
		}
		if !h.canMintPlatform(ctx, ownerID, p) {
			return fmt.Errorf("%s can't be offered to bookers: %s", platformLabel(p), mintHint(p))
		}
	}
	return nil
}

// canMintPlatform reports whether a booking of ownerID's could get a p meeting minted.
func (h *Handler) canMintPlatform(ctx context.Context, ownerID, p string) bool {
	switch p {
	case "google_meet", "teams":
		gc := h.getCal()
		if gc == nil {
			return false
		}
		prov, err := gc.MeetingProvider(ctx, ownerID, p)
		return err == nil && prov != ""
	case "zoom":
		zc := h.getZoom()
		if zc == nil {
			return false
		}
		ok, err := zc.Connected(ctx, ownerID)
		return err == nil && ok
	}
	return false
}

// mintHint says what makes p mintable.
func mintHint(p string) string {
	switch p {
	case "google_meet":
		return "connect a Google calendar first"
	case "teams":
		return "set up the workspace Teams account in Settings → Microsoft Teams, or connect a Microsoft 365 work account"
	case "zoom":
		return "connect your Zoom account on the Calendar page first"
	}
	return "it is not a video platform"
}
