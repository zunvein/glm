package zbridge

import "strings"

// maskSecret renders a credential for logs: the first 7 and last 3 characters
// with the middle elided, or just asterisks when it is too short for that to
// hide anything. The startup banner lands in bridge.log, which users copy and
// paste when asking for help.
func maskSecret(s string) string {
	if len(s) < 16 {
		return strings.Repeat("*", 8)
	}
	return s[:7] + "..." + s[len(s)-3:]
}
