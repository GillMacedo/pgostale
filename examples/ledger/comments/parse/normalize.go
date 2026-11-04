package parse

import "strings"

// normalize lowercases an account name and trims spaces around it, so
// "Acct-1 " and "acct-1" are the same account.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
