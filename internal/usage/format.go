package usage

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Compact writes a token count in at most three significant digits: 950,
// 12.3k, 4.56M, 1.2G.
func Compact(n int64) string {
	if n < 0 {
		return "-" + Compact(-n)
	}
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}
	units := []string{"k", "M", "G", "T"}
	v := float64(n)
	for i, u := range units {
		v /= 1000
		digits := 2
		switch {
		case v >= 100:
			digits = 0
		case v >= 10:
			digits = 1
		}
		p := math.Pow(10, float64(digits))
		r := math.Round(v*p) / p
		if r >= 1000 && i < len(units)-1 {
			continue
		}
		s := strconv.FormatFloat(r, 'f', digits, 64)
		if strings.Contains(s, ".") {
			s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
		}
		return s + u
	}
	return strconv.FormatInt(n, 10)
}

// Amount writes one of a Sum's figures: "?" when none of its turns reported
// anything (never 0), with "≥" when some were cut short or not reported.
func Amount(s Sum, n int64) string {
	if s.Turns > 0 && s.Exact+s.Partial == 0 {
		return "?"
	}
	if !s.Complete() {
		return "≥" + Compact(n)
	}
	return Compact(n)
}

// Cached writes the cache reads of a Sum, "-" when no provider reported any
// cache figures.
func Cached(s Sum) string {
	if !s.Tokens.HasCache {
		return "-"
	}
	return Amount(s, s.Tokens.CacheRead)
}

// Shortfall says why a Sum isn't Complete: "1 turn cut short, 2 not
// reported". It is empty for a complete Sum.
func Shortfall(s Sum) string {
	var parts []string
	if s.Partial > 0 {
		parts = append(parts, plural(s.Partial, "turn")+" cut short")
	}
	if s.Unknown > 0 {
		if len(parts) > 0 {
			parts = append(parts, strconv.Itoa(s.Unknown)+" not reported")
		} else {
			parts = append(parts, plural(s.Unknown, "turn")+" not reported")
		}
	}
	return strings.Join(parts, ", ")
}

// FormatSum is a Sum in one line: "12.3k tokens in 4 turns", "≥12.3k tokens
// in 4 turns (1 turn cut short)", "unknown tokens in 2 turns (2 turns not
// reported)", or "no turns".
func FormatSum(s Sum) string {
	if s.Turns == 0 {
		return "no turns"
	}
	total := Amount(s, s.Tokens.Total())
	if total == "?" {
		total = "unknown"
	}
	out := fmt.Sprintf("%s tokens in %s", total, plural(s.Turns, "turn"))
	if why := Shortfall(s); why != "" {
		out += " (" + why + ")"
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
