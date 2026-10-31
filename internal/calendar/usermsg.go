package calendar

// Wisegrid fork: say why a calendar call failed instead of "could not reach the calendar
// provider" for every failure. A connected Microsoft account without an Exchange Online
// mailbox, a tenant that blocks the permission, and a network outage need three different
// fixes, and only the provider's error says which one applies.

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strings"
)

var (
	providerErrCode = regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`)
	providerErrMsg  = regexp.MustCompile(`"message"\s*:\s*"([^"]*)"`)
	providerStatus  = regexp.MustCompile(`status (\d{3})`)
)

// UserMessage turns a failed provider call into a sentence the calendar's owner can act on.
// It is shown to the signed-in user about their own account, so it may quote the provider.
func UserMessage(err error) string {
	if err == nil {
		return ""
	}
	if IsReauthErr(err) {
		return "This calendar needs reconnecting: disconnect it and connect it again."
	}
	s := err.Error()
	code := firstGroup(providerErrCode, s)
	switch code {
	case "MailboxNotEnabledForRESTAPI", "MailboxNotSupportedForRESTAPI", "ResourceNotFound":
		if strings.Contains(s, "microsoft") {
			return "This Microsoft account has no Exchange Online mailbox, so it has no Outlook calendar Calnode can use. " +
				"Connect an account that has a Microsoft 365 or Outlook.com mailbox (a work account needs an Exchange Online licence)."
		}
	case "ErrorAccessDenied", "Authorization_RequestDenied", "AccessDenied":
		return "The calendar provider refused access to this calendar. " +
			"An administrator of your Microsoft 365 organisation may need to approve Calnode's calendar permission (" + code + ")."
	}
	switch firstGroup(providerStatus, s) {
	case "401":
		return "The calendar provider rejected Calnode's access (401): disconnect the calendar and connect it again."
	case "403":
		return "The calendar provider refused access to this calendar (403)" + detail(code, s) + "."
	case "429":
		return "The calendar provider is limiting requests right now. Try again in a minute."
	case "500", "502", "503", "504":
		return "The calendar provider had a temporary error. Try again shortly."
	}
	if isNetworkErr(err) {
		return "Calnode's server could not reach the calendar provider (network error). Try again shortly."
	}
	return "The calendar provider returned an error" + detail(code, s) + "."
}

func isNetworkErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := err.Error()
	for _, frag := range []string{"no such host", "i/o timeout", "connection refused", "connection reset", "TLS handshake timeout"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// detail is ": <provider message> (<code>)" when the provider sent them, else "".
func detail(code, s string) string {
	msg := firstGroup(providerErrMsg, s)
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	switch {
	case msg != "" && code != "":
		return ": " + msg + " (" + code + ")"
	case msg != "":
		return ": " + msg
	case code != "":
		return " (" + code + ")"
	}
	return ""
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}
