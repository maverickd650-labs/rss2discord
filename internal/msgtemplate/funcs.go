/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package msgtemplate

import (
	"fmt"
	"strings"
	"text/template"

	"github.com/maverickd650/rss2discord-operator/internal/discord"
)

// maxTemplateReplaceBytes bounds each replace result before allocating it,
// including intermediate values in pipelines, well above Discord's limits.
const maxTemplateReplaceBytes = 1024 * 1024

// templateFuncs are the helpers available in message templates. Keep the set
// small and pure: no environment, file, network or reflection access, since
// templates are user-supplied. Arguments are ordered so they pipeline
// ({{.Description | truncate 200}}).
var templateFuncs = template.FuncMap{
	// truncate clamps s to n runes, ending in an ellipsis when it cuts and n > 1.
	// Non-positive limits return empty text.
	"truncate": func(n int, s string) string {
		out, _ := discord.Truncate(s, n)
		return out
	},
	// default returns def when v is blank.
	"default": func(def, v string) string {
		if strings.TrimSpace(v) == "" {
			return def
		}
		return v
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"trim":  strings.TrimSpace,
	// replace replaces all occurrences of old in s, returning an error if the
	// result would exceed maxTemplateReplaceBytes, even in an intermediate pipeline.
	"replace": func(old, new, s string) (string, error) {
		count := strings.Count(s, old)
		remaining := len(s) - count*len(old)
		// Compare before multiplying to avoid overflow. Count handles an
		// empty old string as one match per UTF-8 boundary, like ReplaceAll.
		if remaining > maxTemplateReplaceBytes || (count > 0 && len(new) > (maxTemplateReplaceBytes-remaining)/count) {
			return "", fmt.Errorf("replace: result exceeds maximum size of %d bytes", maxTemplateReplaceBytes)
		}
		return strings.ReplaceAll(s, old, new), nil
	},
	// discordTime renders a Discord dynamic timestamp, e.g.
	// {{.PublishedUnix | discordTime "R"}}. An undated entry
	// (PublishedUnix 0) renders "" rather than "<t:0:R>" (1970).
	// style must be one of t, T, d, D, f, F, or R; other values return an error,
	// including when unix is 0.
	"discordTime": func(style string, unix int64) (string, error) {
		if !strings.Contains("tTdDfFR", style) || len(style) != 1 {
			return "", fmt.Errorf("discordTime: unknown style %q (want one of t T d D f F R)", style)
		}
		if unix == 0 {
			return "", nil
		}
		return fmt.Sprintf("<t:%d:%s>", unix, style), nil
	},
}
