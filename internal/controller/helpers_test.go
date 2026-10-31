package controller

// overflowingInterval passes the CRD's Pattern and MaxLength yet is rejected
// by time.ParseDuration (overflow); notParsable is rejected outright.
const (
	overflowingInterval = "9999999999999h"
	notParsable         = "nope"
)

// shortContent is reused across test cases below.
const shortContent = "short"

// Shared fixtures that several tests below (and fuzz_test.go) repeat.
const (
	jsAlertURL        = "javascript:alert(1)"
	discordRelativeTS = "<t:1714979289:R>"
	imageFixtureURL   = "https://x/a.png"
)

// excerptText is reused across the "continue reading" stripHTML test cases
// below.
const excerptText = "Some excerpt text"

// Template sources shared across tests.
const (
	tmplTitle        = "{{.Title}}"
	tmplMissingField = "{{.DoesNotExist}}"
	tmplSafeText     = "safe"
)
