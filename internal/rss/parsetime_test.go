package rss

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// referenceParseTime is the original parseTime: nine layouts tried strictly
// in order. parseTime now picks a layout group from the value's shape to
// avoid a failed time.Parse (and its error allocation) per date, and must
// accept exactly the same inputs and return the same instants.
func referenceParseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	layouts := []string{
		time.RFC3339,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.ANSIC,
		"Mon, 2 Jan 2006 15:04:05 MST",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported time format %q", value)
}

const (
	isoDateTime = "2016-11-22T08:29:01Z"
	isoDate     = "2016-11-22"
)

var parseTimeCorpus = []string{
	"",
	" \t ",
	isoDateTime,
	"2015-10-21T07:28:00+02:00",
	"2015-10-21T07:28:00.123456Z",
	"2015-10-21 07:28:00",
	isoDate,
	"Tue, 22 Nov 2016 08:29:01 +0000",
	"Wed, 21 Oct 2015 07:28:00 -0700",
	"Wed, 21 Oct 2015 07:28:00 UTC",
	"Wed, 21 Oct 2015 07:28:00 GMT",
	"Wed, 21 Oct 2015 07:28:00 EST",
	"Wed, 2 Jan 2006 15:04:05 MST",
	"Mon, 02 Jan 06 15:04 MST",
	"02 Jan 06 15:04 MST",
	"02 Jan 06 15:04 -0700",
	"Mon Jan  2 15:04:05 2006",
	"Mon Jan 2 15:04:05 2006",
	"not a date at all",
	"2015-13-45",
	"2015-10-21T",
	"21 Oct 2015",
	"Wed",
	"20151021",
	"abcd-10-21", // dash at index 4 but non-digit year
	"201x-10-21",
	"12-3",
	"2015/10/21",
	"\t2015-10-21T07:28:00Z\n",
}

func TestParseTimeMatchesReference(t *testing.T) {
	for _, in := range parseTimeCorpus {
		assertMatchesReference(t, in)
	}
}

// assertMatchesReference checks parseTime against referenceParseTime for in.
func assertMatchesReference(t *testing.T, in string) {
	t.Helper()
	got, gotErr := parseTime(in)
	want, wantErr := referenceParseTime(in)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("parseTime(%q) err = %v, reference err = %v", in, gotErr, wantErr)
	}
	if !got.Equal(want) {
		t.Fatalf("parseTime(%q) = %v, reference = %v", in, got, want)
	}
}

func FuzzParseTime(f *testing.F) {
	for _, in := range parseTimeCorpus {
		f.Add(in)
	}
	f.Fuzz(assertMatchesReference)
}

func BenchmarkParseTime(b *testing.B) {
	for _, tc := range []struct{ name, in string }{
		{"RFC1123Z", "Tue, 22 Nov 2016 08:29:01 +0000"},
		{"RFC1123", "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"RFC3339", isoDateTime},
		{"dateOnly", isoDate},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _ = parseTime(tc.in)
			}
		})
	}
}

func TestLooksISO(t *testing.T) {
	cases := map[string]bool{
		isoDate:            true,
		isoDateTime:        true,
		"abcd-10-21":       false,
		"201x-10-21":       false,
		"2016/11/22":       false,
		"12-3":             false,
		"":                 false,
		"Tue, 22 Nov 2016": false,
	}
	for in, want := range cases {
		if got := looksISO(in); got != want {
			t.Errorf("looksISO(%q) = %v, want %v", in, got, want)
		}
	}
}

// instant0728 is 2015-10-21 07:28:00 UTC, which several cases below resolve to.
const instant0728 = "2015-10-21T07:28:00Z"

func TestParseTime(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    string // the expected instant in UTC, as RFC 3339
		wantErr bool
	}{
		{name: "blank", value: "   ", want: "0001-01-01T00:00:00Z"}, // the zero time, distinct from an error
		{name: "RFC3339", value: instant0728, want: instant0728},
		{name: "RFC1123Z", value: "Wed, 21 Oct 2015 07:28:00 +0000", want: instant0728},
		{name: "RFC1123Z with offset", value: "Wed, 21 Oct 2015 07:28:00 -0700", want: "2015-10-21T14:28:00Z"},
		{name: "RFC1123", value: "Wed, 21 Oct 2015 07:28:00 UTC", want: instant0728},
		{name: "single-digit day", value: "Wed, 2 Jan 2006 15:04:05 MST", want: "2006-01-02T15:04:05Z"},
		{name: "date only", value: "2015-10-21", want: "2015-10-21T00:00:00Z"},
		{name: "datetime no zone", value: "2015-10-21 07:28:00", want: instant0728},
		{name: "unsupported", value: "not a date at all", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTime(tc.value)
			assertField(t, "error for "+tc.value, err != nil, tc.wantErr)
			if err != nil {
				return
			}
			assertField(t, "instant", got.Format(time.RFC3339), tc.want)
			assertField(t, "location", got.Location(), time.UTC)
		})
	}
}
