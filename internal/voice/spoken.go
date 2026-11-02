// Package voice implements the VOICE_CALL channel: call placement and policy,
// the call scheduler and sweeper, VoIP push signaling, and the scripted
// dialogue the phone speaks with Gobbie's on-device voice.
package voice

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Spoken turns display text into what the voice should say. It is a port of
// Gobbie's SpokenText (OmniWeather Runtime/Voice/SpokenText.cs): Kokoro reads
// digits, clock times and symbols badly, so they are written out in words.
func Spoken(display string) string {
	if display == "" {
		return ""
	}
	s := display
	s = clockRe.ReplaceAllStringFunc(s, func(m string) string {
		g := clockRe.FindStringSubmatch(m)
		h, _ := strconv.Atoi(g[1])
		mi, _ := strconv.Atoi(g[2])
		return Words(h) + " " + minutesWords(mi) + meridiem(g[3])
	})
	s = hourRe.ReplaceAllStringFunc(s, func(m string) string {
		g := hourRe.FindStringSubmatch(m)
		h, _ := strconv.Atoi(g[1])
		return Words(h) + meridiem(g[2])
	})
	s = percentRe.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.Atoi(percentRe.FindStringSubmatch(m)[1])
		return Words(n) + " percent"
	})
	s = numberRe.ReplaceAllStringFunc(s, func(m string) string {
		n, err := strconv.Atoi(m)
		if err != nil || n > 999999 || n < -999999 {
			return m
		}
		return Words(n)
	})
	// Dashes and ellipses read as odd pauses: a comma and a full stop say it better.
	s = strings.NewReplacer(" — ", ", ", "—", ", ", " – ", ", ", "…", ".", "%", " percent", "&", " and ").Replace(s)
	s = spacesRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return sentenceStartRe.ReplaceAllStringFunc(s, strings.ToUpper)
}

var (
	clockRe         = regexp.MustCompile(`\b(\d{1,2}):(\d{2})\s*(AM|PM)\b`)
	hourRe          = regexp.MustCompile(`\b(\d{1,2})\s*(AM|PM)\b`)
	percentRe       = regexp.MustCompile(`(\d+)\s*%`)
	numberRe        = regexp.MustCompile(`-?\d+`)
	spacesRe        = regexp.MustCompile(`\s{2,}`)
	sentenceStartRe = regexp.MustCompile(`(^|[.!?]\s+)[a-z]`)
)

func meridiem(ampm string) string {
	if ampm == "AM" {
		return " A M"
	}
	return " P M"
}

func minutesWords(m int) string {
	switch {
	case m == 0:
		return ""
	case m < 10:
		return "oh " + Words(m)
	default:
		return Words(m)
	}
}

var (
	smallWords = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
	tensWords = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
)

// Words spells an integer: "twenty-two", "one hundred and five".
func Words(n int) string {
	if n < 0 {
		return "minus " + Words(-n)
	}
	if n < 20 {
		return smallWords[n]
	}
	if n < 100 {
		if n%10 != 0 {
			return tensWords[n/10] + "-" + smallWords[n%10]
		}
		return tensWords[n/10]
	}
	var b strings.Builder
	if n >= 1000 {
		b.WriteString(Words(n/1000) + " thousand")
		n %= 1000
		if n == 0 {
			return b.String()
		}
		if n < 100 {
			b.WriteString(" and ")
		} else {
			b.WriteString(" ")
		}
	}
	if n >= 100 {
		b.WriteString(smallWords[n/100] + " hundred")
		n %= 100
		if n == 0 {
			return b.String()
		}
		b.WriteString(" and ")
	}
	b.WriteString(Words(n))
	return b.String()
}

// SayDuration phrases a duration the way a person would say it: "ten
// minutes", "an hour", "an hour and a half", "two hours and five minutes".
func SayDuration(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	if m < 1 {
		m = 1
	}
	h, mm := m/60, m%60
	switch {
	case h == 0:
		return plural(mm, "minute")
	case mm == 0:
		if h == 1 {
			return "an hour"
		}
		return plural(h, "hour")
	case mm == 30:
		if h == 1 {
			return "an hour and a half"
		}
		return Words(h) + " and a half hours"
	default:
		hs := plural(h, "hour")
		if h == 1 {
			hs = "an hour"
		}
		return hs + " and " + plural(mm, "minute")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "one " + unit
	}
	return Words(n) + " " + unit + "s"
}

// ClockText is the display form of a time of day in loc ("5:42 PM"), with
// "tomorrow" or the weekday when it is not today.
func ClockText(t, now time.Time, loc *time.Location) string {
	lt, ln := t.In(loc), now.In(loc)
	s := lt.Format("3:04 PM")
	if strings.HasSuffix(s, ":00 AM") || strings.HasSuffix(s, ":00 PM") {
		s = lt.Format("3 PM")
	}
	if lt.Hour() == 12 && lt.Minute() == 0 {
		s = "noon"
	}
	if lt.Hour() == 0 && lt.Minute() == 0 {
		s = "midnight"
	}
	dayDiff := civilDays(lt) - civilDays(ln)
	switch {
	case dayDiff == 0:
		return s
	case dayDiff == 1:
		return "tomorrow at " + s
	case dayDiff > 1 && dayDiff < 7:
		return fmt.Sprintf("%s at %s", lt.Weekday(), s)
	default:
		return fmt.Sprintf("%s %d at %s", lt.Month(), lt.Day(), s)
	}
}

func civilDays(t time.Time) int {
	y, m, d := t.Date()
	return int(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}
