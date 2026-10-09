package schoology

import "strings"

// NormalizeHost reduces an operator-supplied Schoology tenant value to the
// bare host the schoology-go login and client paths expect
// ("yourschool.schoology.com"). Operators routinely store the full URL
// ("https://yourschool.schoology.com/"); the login path builds
// "https://" + host itself, so a scheme left in place yields a broken URL.
func NormalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.IndexAny(h, "/?#"); i >= 0 {
		h = h[:i]
	}
	return strings.ToLower(h)
}
