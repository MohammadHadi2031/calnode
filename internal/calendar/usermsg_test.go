package calendar

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestUserMessage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"mailbox-less Microsoft account",
			errors.New(`microsoft: list calendars status 404: {"error":{"code":"MailboxNotEnabledForRESTAPI","message":"The mailbox is either inactive, soft-deleted, or is hosted on-premise."}}`),
			"no Exchange Online mailbox"},
		{"tenant blocks the permission",
			errors.New(`microsoft: list calendars status 403: {"error":{"code":"ErrorAccessDenied","message":"Access is denied. Check credentials and try again."}}`),
			"administrator of your Microsoft 365 organisation"},
		{"dead grant", errors.New(`oauth2: "invalid_grant" "AADSTS700082: expired"`), "needs reconnecting"},
		{"unauthorized", errors.New(`microsoft: list calendars status 401: `), "(401)"},
		{"throttled", errors.New(`microsoft: list calendars status 429: {}`), "limiting requests"},
		{"provider outage", errors.New(`microsoft: list calendars status 503: {}`), "temporary error"},
		{"network", fmt.Errorf("microsoft: list calendars call: %w", context.DeadlineExceeded), "could not reach"},
		{"dns", errors.New(`Get "https://graph.microsoft.com/v1.0/me/calendars": dial tcp: lookup graph.microsoft.com: no such host`), "could not reach"},
		{"anything else quotes the provider",
			errors.New(`microsoft: list calendars status 400: {"error":{"code":"BadRequest","message":"Invalid filter clause"}}`),
			"Invalid filter clause (BadRequest)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UserMessage(c.err); !strings.Contains(got, c.want) {
				t.Fatalf("UserMessage = %q, want it to contain %q", got, c.want)
			}
		})
	}
	if UserMessage(nil) != "" {
		t.Fatal("nil error must give no message")
	}
}
