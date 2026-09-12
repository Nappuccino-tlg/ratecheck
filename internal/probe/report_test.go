package probe

import (
	"strings"
	"testing"
)

// flatten collapses the report's wrapping so assertions can be about what it says.
func flatten(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

func TestALeakVerdictNamesTheFix(t *testing.T) {
	// A linter that reports a problem and leaves the answer as an exercise gets silenced
	// by the first person in a hurry. This one has to say what to do instead.
	report := Report{
		Discovery: Discovery{Limit: 20, Rejected: true, Took: "1.2s"},
		Burst:     &Burst{Fired: 30, Allowed: 29, Denied: 1, Budget: 19, SpreadMS: 3.4, Warm: 30},
		Verdict:   Leaked,
	}

	var out strings.Builder
	report.WriteText(&out)
	text := out.String()

	for _, want := range []string{"LEAKED", "check-then-increment", "INCR", "Lua", "RETURNING"} {
		if !strings.Contains(flatten(text), want) {
			t.Errorf("the leak report never mentions %q:\n%s", want, text)
		}
	}
}

func TestAHeldVerdictSaysWhatWasProved(t *testing.T) {
	report := Report{
		Discovery: Discovery{Limit: 20, Rejected: true, Took: "1.1s"},
		Burst:     &Burst{Fired: 30, Allowed: 19, Denied: 11, Budget: 19, SpreadMS: 2.1, Warm: 30},
		Verdict:   Held,
	}

	var out strings.Builder
	report.WriteText(&out)
	text := out.String()

	if !strings.Contains(text, "HELD") {
		t.Errorf("missing the verdict:\n%s", text)
	}
	// Flattened first: the prose is wrapped to the terminal, so asserting on a phrase that
	// happens to straddle a line break would be testing the wrapper, not the words.
	if !strings.Contains(flatten(text), "one request at a time cannot tell you") {
		t.Errorf("does not say what the sequential test could not have shown:\n%s", text)
	}
}

func TestAnUnknownVerdictCarriesItsNote(t *testing.T) {
	report := Report{
		Discovery: Discovery{Limit: 5, Rejected: true, Took: "40ms"},
		Verdict:   Unknown,
		Note:      "Raise -burst above the limit.",
	}

	var out strings.Builder
	report.WriteText(&out)
	text := out.String()

	if !strings.Contains(text, "UNKNOWN") || !strings.Contains(text, "-burst") {
		t.Errorf("the note did not survive into the output:\n%s", text)
	}
}

func TestTheApisOwnHeadersAreShownBesideTheMeasurement(t *testing.T) {
	// Printing both is the point: an API that advertises a limit of 100 and starts
	// rejecting at 20 has a bug worth seeing, and neither number alone shows it.
	report := Report{
		Discovery: Discovery{
			Limit: 20, Rejected: true, Took: "1s", RetryAfter: "60",
			Claim: HeaderClaim{Limit: "100", Remaining: "0", Reset: "1757000000"},
		},
		Verdict: Unknown,
	}

	var out strings.Builder
	report.WriteText(&out)
	text := out.String()

	if !strings.Contains(text, "limit 100") {
		t.Errorf("the advertised limit is missing:\n%s", text)
	}
	if !strings.Contains(text, "20 requests, then rejected") {
		t.Errorf("the measured limit is missing:\n%s", text)
	}
	if !strings.Contains(text, "Retry-After  60") {
		t.Errorf("Retry-After is missing:\n%s", text)
	}
}

func TestNoLimitFoundReadsAsSuch(t *testing.T) {
	report := Report{
		Discovery: Discovery{Limit: 200, Rejected: false, Took: "3s"},
		Verdict:   Unknown,
		Note:      "200 requests in a row were all allowed.",
	}

	var out strings.Builder
	report.WriteText(&out)

	if !strings.Contains(out.String(), "none found in 200 requests") {
		t.Errorf("unexpected wording:\n%s", out.String())
	}
}

func TestWrapKeepsLinesReadable(t *testing.T) {
	long := strings.Repeat("word ", 60)
	for _, line := range strings.Split(wrap(long), "\n") {
		if len(line) > 80 {
			t.Errorf("line is %d characters: %q", len(line), line)
		}
		if !strings.HasPrefix(line, "          ") {
			t.Errorf("line is not indented to match the verdict: %q", line)
		}
	}
}

func TestASpreadOutBurstIsNotConcurrent(t *testing.T) {
	// The guard that stops this tool giving a confident answer from a test that did not
	// happen. Requests spread over a second cannot catch a race that lasts milliseconds.
	if (Burst{Fired: 30, SpreadMS: 950}).Concurrent() {
		t.Error("950ms of spread was accepted as concurrent")
	}
	if !(Burst{Fired: 30, SpreadMS: 4.2}).Concurrent() {
		t.Error("4.2ms of spread was rejected as not concurrent")
	}
	if (Burst{Fired: 0}).Concurrent() {
		t.Error("a burst that never fired was accepted as concurrent")
	}
}

func TestParseHeader(t *testing.T) {
	cases := []struct {
		in          string
		name, value string
		wantErr     bool
	}{
		{in: "Authorization: Bearer abc", name: "Authorization", value: "Bearer abc"},
		{in: "X-Key:no-space", name: "X-Key", value: "no-space"},
		{in: "X-Empty:", name: "X-Empty", value: ""},
		// A bearer token has colons in it often enough that splitting on the last one, or
		// on all of them, would quietly mangle the credential.
		{in: "Cookie: a=1:2:3", name: "Cookie", value: "a=1:2:3"},
		{in: "no-colon-here", wantErr: true},
		{in: ": value", wantErr: true},
	}
	for _, c := range cases {
		name, value, err := ParseHeader(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseHeader(%q) should have failed", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseHeader(%q): %v", c.in, err)
			continue
		}
		if name != c.name || value != c.value {
			t.Errorf("ParseHeader(%q) = %q, %q; want %q, %q", c.in, name, value, c.name, c.value)
		}
	}
}

func TestAllowedTreatsAServerErrorAsAllowed(t *testing.T) {
	// A limiter that is failing open is letting requests through, and calling that
	// "denied" would hide the very failure worth knowing about.
	if !(Result{Status: 500}).Allowed(429) {
		t.Error("a 500 was counted as denied")
	}
	if (Result{Status: 429}).Allowed(429) {
		t.Error("a 429 was counted as allowed")
	}
	if (Result{Err: errDial}).Allowed(429) {
		t.Error("a failed request was counted as allowed")
	}
}

var errDial = &dialError{}

type dialError struct{}

func (*dialError) Error() string { return "dial failed" }

func TestShapeNamesAPartialRefill(t *testing.T) {
	// Between the two clean answers there is a window caught mid-refill, and saying so is
	// more honest than rounding it to whichever named shape is nearer.
	cases := map[string]struct{ restored, limit int }{
		"the whole allowance of 20 came back at once -- a fixed window":            {20, 20},
		"quota comes back a request at a time -- a sliding window or token bucket": {1, 20},
		"7 of 20 came back -- the window is partway through refilling":             {7, 20},
	}
	for want, c := range cases {
		if got := shapeOf(c.restored, c.limit); got != want {
			t.Errorf("shapeOf(%d, %d) = %q, want %q", c.restored, c.limit, got, want)
		}
	}
}
