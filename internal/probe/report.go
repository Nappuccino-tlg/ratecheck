package probe

import (
	"fmt"
	"io"
	"strings"
)

// WriteText prints the report the way someone reading a terminal wants it.
//
// Every finding says what to do about it. A tool that reports a problem and leaves the
// explanation as an exercise gets ignored by the first person in a hurry, and a race in a
// rate limiter is exactly the finding that sounds theoretical until it is not.
func (r Report) WriteText(w io.Writer) {
	fmt.Fprintln(w)

	d := r.Discovery
	if d.Rejected {
		fmt.Fprintf(w, "  Limit        %d requests, then rejected (%s)\n", d.Limit, d.Took)
	} else {
		fmt.Fprintf(w, "  Limit        none found in %d requests (%s)\n", d.Limit, d.Took)
	}
	if claim := d.headerLine(); claim != "" {
		fmt.Fprintf(w, "  Headers      %s\n", claim)
	}
	if d.RetryAfter != "" {
		fmt.Fprintf(w, "  Retry-After  %s\n", d.RetryAfter)
	}
	if r.Window != nil {
		// The shape is only known when the limiter held: with a leak, the number it would
		// be derived from is the leak itself. Then the wait is all there is to say.
		if r.Window.Shape != "" {
			fmt.Fprintf(w, "  Window       %s, after %s\n", r.Window.Shape, r.Window.Waited)
		} else {
			fmt.Fprintf(w, "  Window       quota returned after %s\n", r.Window.Waited)
		}
	}
	if b := r.Burst; b != nil {
		fmt.Fprintf(w, "  Burst        %d at once, %d allowed, %d rejected", b.Fired, b.Allowed, b.Denied)
		if b.Failed > 0 {
			fmt.Fprintf(w, ", %d failed", b.Failed)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  Overlap      all %d hit the wire within %.1fms, %d on a warm connection\n",
			b.Fired, b.SpreadMS, b.Warm)
	}

	fmt.Fprintln(w)
	for _, line := range r.verdictLines() {
		fmt.Fprintln(w, line)
	}
	fmt.Fprintln(w)
}

func (d Discovery) headerLine() string {
	var parts []string
	if d.Claim.Limit != "" {
		parts = append(parts, "limit "+d.Claim.Limit)
	}
	if d.Claim.Remaining != "" {
		parts = append(parts, "remaining "+d.Claim.Remaining)
	}
	if d.Claim.Reset != "" {
		parts = append(parts, "reset "+d.Claim.Reset)
	}
	return strings.Join(parts, ", ")
}

func (r Report) verdictLines() []string {
	switch r.Verdict {
	case Leaked:
		b := r.Burst
		return []string{
			fmt.Sprintf("  LEAKED  A limit of %d let %d requests through when they arrived together.",
				r.Discovery.Limit, b.Allowed),
			"",
			wrap("This is what a check-then-increment limiter looks like from outside. Every " +
				"request in the burst read the same counter before any of them wrote it back, so " +
				"they all saw room. Under real load the effective limit is not the configured " +
				"one, it is however many requests happen to overlap."),
			"",
			wrap("The fix is to make the read and the write one operation the server cannot " +
				"interleave: INCR with an expiry, or a Lua script, or a single UPDATE ... " +
				"RETURNING. Not a GET followed by a SET."),
		}
	case Held:
		return []string{
			fmt.Sprintf("  HELD    %d at once, %d allowed, and the budget was %d.",
				r.Burst.Fired, r.Burst.Allowed, r.Burst.Budget),
			"",
			wrap("The limiter counted concurrent requests the same way it counted sequential " +
				"ones, which is the property that a test sending one request at a time cannot " +
				"tell you anything about."),
		}
	default:
		lines := []string{"  UNKNOWN  This run could not answer the question."}
		if r.Note != "" {
			lines = append(lines, "", wrap(r.Note))
		}
		return lines
	}
}

// wrap folds text to something readable in a terminal, indented to line up with the
// verdict above it.
func wrap(text string) string {
	const width = 76
	const indent = "          "

	var out strings.Builder
	line := indent
	for _, word := range strings.Fields(text) {
		if len(line)+len(word)+1 > width && line != indent {
			out.WriteString(line + "\n")
			line = indent
		}
		if line != indent {
			line += " "
		}
		line += word
	}
	out.WriteString(line)
	return out.String()
}
