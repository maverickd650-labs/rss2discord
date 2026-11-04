package controller

import (
	"net/http"

	"github.com/maverickd650-labs/rss2discord/internal/discord"
)

// http500Status is the HTTP status string for a 500 Internal Server Error response,
// shared across test cases to satisfy goconst.
const http500Status = "500 Internal Server Error"

// errInternalServer is a common 500 error used in fakeSender setups.
var errInternalServer = &discord.HTTPStatusError{
	StatusCode: http.StatusInternalServerError,
	Status:     http500Status,
}

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
