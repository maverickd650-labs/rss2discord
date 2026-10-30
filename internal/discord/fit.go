package discord

// Discord's webhook length limits for the fields rendered from feed content.
// Discord rejects a message exceeding any of them outright rather than
// truncating, so Fit enforces all of them.
const (
	// MaxContentLength is the cap on a message's plain-text content.
	MaxContentLength = 2000
	// MaxEmbedTitleLength is the cap on an embed's title.
	MaxEmbedTitleLength = 256
	// MaxEmbedDescriptionLength is the cap on an embed's description.
	MaxEmbedDescriptionLength = 4096
	// MaxThreadNameLength is the cap on a forum thread name.
	MaxThreadNameLength = 100
	// MaxEmbedTotalLength is the cap on the combined character count of
	// title + description + footer.text + author.name (+ field name/value,
	// which this package doesn't use) across all embeds in one message.
	// Discord rejects the whole call if exceeded, even though each field
	// individually stays under its own limit.
	MaxEmbedTotalLength = 6000
)

// Fit applies the limits above to msg and returns the fitted message plus the
// total number of runes trimmed (0 when nothing was). Order: Content,
// Embed.Title, Embed.Description, then the combined embed clamp (which trims
// Description further, measured after the per-field trims), then ThreadName.
// Embed.AuthorName, Embed.FooterText and Username come from FeedGroup config,
// which CRD validation already caps at Discord's limits, so Fit leaves them
// alone (they still count toward the combined embed clamp).
// Fit is idempotent and never mutates msg.Embed: the embed is copied.
func Fit(msg Message) (Message, int) {
	var trimmed, n int

	msg.Content, n = Truncate(msg.Content, MaxContentLength)
	trimmed += n

	if msg.Embed != nil {
		e := *msg.Embed
		e.Title, n = Truncate(e.Title, MaxEmbedTitleLength)
		trimmed += n
		e.Description, n = Truncate(e.Description, MaxEmbedDescriptionLength)
		trimmed += n
		e, n = clampEmbedTotalLength(e)
		trimmed += n
		msg.Embed = &e
	}

	msg.ThreadName, n = Truncate(msg.ThreadName, MaxThreadNameLength)
	trimmed += n
	return msg, trimmed
}

// clampEmbedTotalLength shrinks Description, the field with the most slack,
// until title+description+footer+author fits within MaxEmbedTotalLength, and
// reports how many runes it cut. Title/footer/author are short in practice, so
// trimming the (often long) description is the least surprising way to enforce
// the limit. If the other fields alone exceed the cap, Description is emptied
// and the remainder is left as is.
func clampEmbedTotalLength(e Embed) (Embed, int) {
	total := len([]rune(e.Title)) + len([]rune(e.Description)) + len([]rune(e.FooterText)) + len([]rune(e.AuthorName))
	if total <= MaxEmbedTotalLength {
		return e, 0
	}
	descLen := len([]rune(e.Description))
	e.Description, _ = Truncate(e.Description, max(descLen-(total-MaxEmbedTotalLength), 0))
	return e, min(total-MaxEmbedTotalLength, descLen)
}

// Truncate trims s to at most max runes, since Discord rejects over-long
// fields outright. The second return value is how many runes were cut (0 if s
// already fit). A cut ends in an ellipsis when max > 1; max == 1 keeps the
// first rune and max <= 0 returns empty text. The count is the original rune
// count minus max, floored at zero, so a negative max increases it.
func Truncate(s string, max int) (string, int) {
	// len(s) >= rune count, so a byte length within max can't overflow; this
	// skips the []rune allocation for the overwhelmingly common case.
	if len(s) <= max {
		return s, 0
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s, 0
	}
	overflow := len(runes) - max
	if max <= 0 {
		return "", overflow
	}
	if max == 1 {
		return string(runes[:1]), overflow
	}
	return string(runes[:max-1]) + "…", overflow
}
