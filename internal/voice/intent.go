package voice

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// IntentKind is what the recipient asked for on a call.
type IntentKind string

const (
	IntentRepeat      IntentKind = "repeat"
	IntentExplain     IntentKind = "explain"
	IntentIdentify    IntentKind = "identify"
	IntentAcknowledge IntentKind = "acknowledge"
	IntentDismiss     IntentKind = "dismiss"
	IntentSnooze      IntentKind = "snooze"
	IntentReschedule  IntentKind = "reschedule"
	IntentEnd         IntentKind = "end"
	IntentUnknown     IntentKind = "unknown"
	IntentSilence     IntentKind = "silence"
)

// Intent is an interpreted utterance. For snooze, After is set; for
// reschedule, At. Nothing here is trusted: the service validates every action
// against policy before acting on it.
type Intent struct {
	Kind  IntentKind    `json:"kind"`
	After time.Duration `json:"after,omitempty"`
	At    time.Time     `json:"at,omitempty"`
}

// DefaultSnooze is used for "later", "snooze" and other snoozes with no length.
const DefaultSnooze = 10 * time.Minute

// ParseIntent interprets a transcript deterministically. now and loc are the
// recipient's present and time zone, used for times of day ("at five").
func ParseIntent(transcript string, now time.Time, loc *time.Location) Intent {
	t := normalize(transcript)
	if t == "" {
		return Intent{Kind: IntentSilence}
	}
	// Anything with a time or a length is about when to call back, whatever
	// else it says ("no, give me ten more minutes").
	if at, ok := parseClockTime(t, now, loc); ok {
		return Intent{Kind: IntentReschedule, At: at}
	}
	if d, ok := parseDuration(t); ok {
		return Intent{Kind: IntentSnooze, After: d}
	}
	switch {
	case hasAny(t, "snooze", "later", "not now", "in a bit", "in a while", "few more minutes", "give me a bit",
		"call me back", "call back", "remind me again", "five more", "ten more", "more minutes", "sleep"):
		return Intent{Kind: IntentSnooze, After: DefaultSnooze}
	case hasAny(t, "repeat", "say that again", "say again", "say it again", "come again", "pardon", "one more time",
		"what did you say", "didn't catch", "didnt catch", "didn't hear", "didnt hear") || isAny(t, "what", "huh", "sorry", "what was that"):
		return Intent{Kind: IntentRepeat}
	case hasAny(t, "who is this", "who's this", "who is calling", "who's calling", "who are you", "whos calling"):
		return Intent{Kind: IntentIdentify}
	case hasAny(t, "explain", "what is this about", "what's this about", "what is it about", "what's it about",
		"tell me more", "more detail", "details", "more info", "why", "what for", "what do you mean", "elaborate"):
		return Intent{Kind: IntentExplain}
	case hasAny(t, "dismiss", "cancel", "never mind", "nevermind", "forget it", "forget about it", "don't remind",
		"dont remind", "stop reminding", "not interested", "leave me alone", "go away", "don't call", "dont call"):
		return Intent{Kind: IntentDismiss}
	case hasAny(t, "bye", "good bye", "hang up", "that's all", "that is all", "end call", "end the call") || isAny(t, "no", "nope", "nah"):
		return Intent{Kind: IntentEnd}
	case hasAny(t, "ok", "okay", "got it", "thanks", "thank you", "will do", "i'm up", "i am up", "im up", "i'm awake",
		"on it", "sure", "yes", "yeah", "yep", "acknowledge", "roger", "understood", "alright", "all right",
		"fine", "done", "i know", "noted", "on my way", "sounds good"):
		return Intent{Kind: IntentAcknowledge}
	}
	return Intent{Kind: IntentUnknown}
}

var (
	punctRe = regexp.MustCompile(`[^a-z0-9:' ]+`)
	wsRe    = regexp.MustCompile(`\s+`)
)

func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("a.m.", "am", "p.m.", "pm", "’", "'", "-", " ").Replace(s)
	s = punctRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

// hasAny reports whether any phrase occurs in t as whole words.
func hasAny(t string, phrases ...string) bool {
	padded := " " + t + " "
	for _, p := range phrases {
		if strings.Contains(padded, " "+p+" ") {
			return true
		}
	}
	return false
}

func isAny(t string, words ...string) bool {
	for _, w := range words {
		if t == w {
			return true
		}
	}
	return false
}

var numberWords = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14,
	"fifteen": 15, "sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20,
	"thirty": 30, "forty": 40, "fifty": 50, "sixty": 60, "ninety": 90, "couple": 2, "few": 3,
}

// number reads a number at words[i:], digits or words ("twenty five"),
// returning it and how many words it used.
func number(words []string, i int) (int, int) {
	if i >= len(words) {
		return 0, 0
	}
	if n, err := strconv.Atoi(words[i]); err == nil {
		return n, 1
	}
	n, ok := numberWords[words[i]]
	if !ok {
		return 0, 0
	}
	used := 1
	if n >= 20 && n < 100 && i+1 < len(words) {
		if u, ok := numberWords[words[i+1]]; ok && u < 10 && u > 0 && words[i+1] != "a" && words[i+1] != "an" {
			n += u
			used++
		}
	}
	return n, used
}

var durationUnits = map[string]time.Duration{
	"minute": time.Minute, "minutes": time.Minute, "min": time.Minute, "mins": time.Minute,
	"hour": time.Hour, "hours": time.Hour, "hr": time.Hour, "hrs": time.Hour,
	"second": time.Second, "seconds": time.Second,
}

// parseDuration finds a length of time: "ten more minutes", "15 min",
// "half an hour", "an hour and a half", "a couple of hours".
func parseDuration(t string) (time.Duration, bool) {
	switch {
	case hasAny(t, "an hour and a half", "one hour and a half", "hour and a half"):
		return 90 * time.Minute, true
	case hasAny(t, "half an hour", "half hour", "thirty minutes"):
		return 30 * time.Minute, true
	case hasAny(t, "quarter of an hour", "quarter hour", "a quarter hour"):
		return 15 * time.Minute, true
	}
	words := strings.Fields(t)
	for i := range words {
		n, used := number(words, i)
		if used == 0 {
			continue
		}
		j := i + used
		// skip fillers between the number and the unit: "ten more minutes", "a couple of hours"
		for j < len(words) && (words[j] == "more" || words[j] == "of" || words[j] == "extra" || words[j] == "other") {
			j++
		}
		if j < len(words) {
			if unit, ok := durationUnits[words[j]]; ok {
				d := time.Duration(n) * unit
				if d < time.Minute {
					d = time.Minute
				}
				return d, true
			}
		}
	}
	return 0, false
}

// parseClockTime finds a time of day: "at five", "at 5:30 pm", "at noon",
// "tomorrow at nine", "at five thirty". It returns the next such time after now.
func parseClockTime(t string, now time.Time, loc *time.Location) (time.Time, bool) {
	tomorrow := hasAny(t, "tomorrow")
	words := strings.Fields(t)
	for i, w := range words {
		if w != "at" || i+1 >= len(words) {
			continue
		}
		rest := words[i+1:]
		if rest[0] == "noon" || rest[0] == "midday" {
			return nextClock(12, 0, now, loc, tomorrow), true
		}
		if rest[0] == "midnight" {
			return nextClock(0, 0, now, loc, true), true
		}
		h, m, used, ok := clockWords(rest)
		if !ok {
			continue
		}
		meridiem := ""
		if used < len(rest) {
			switch rest[used] {
			case "am", "pm":
				meridiem = rest[used]
			case "in":
				if used+2 < len(rest) && rest[used+1] == "the" {
					switch rest[used+2] {
					case "morning":
						meridiem = "am"
					case "afternoon", "evening":
						meridiem = "pm"
					}
				}
			}
		}
		if meridiem == "" && hasAny(t, "tonight", "this evening", "this afternoon") {
			meridiem = "pm"
		}
		if meridiem == "" && hasAny(t, "morning") {
			meridiem = "am"
		}
		if h < 1 || h > 23 || m > 59 {
			continue
		}
		if h > 12 {
			return nextClock(h, m, now, loc, tomorrow), true
		}
		switch meridiem {
		case "am":
			return nextClock(h%12, m, now, loc, tomorrow), true
		case "pm":
			return nextClock(h%12+12, m, now, loc, tomorrow), true
		}
		if tomorrow {
			// "tomorrow at nine" is the morning, "tomorrow at two" the afternoon.
			if h >= 7 && h <= 11 {
				return nextClock(h, m, now, loc, true), true
			}
			return nextClock(h%12+12, m, now, loc, true), true
		}
		// No meridiem: the next time the clock shows h:m.
		a := nextClock(h%12, m, now, loc, false)
		b := nextClock(h%12+12, m, now, loc, false)
		if b.Before(a) {
			return b, true
		}
		return a, true
	}
	return time.Time{}, false
}

// clockWords reads "5", "5:30", "five", "five thirty", "five oh five".
func clockWords(w []string) (h, m, used int, ok bool) {
	if hh, mm, found := strings.Cut(w[0], ":"); found {
		h, err1 := strconv.Atoi(hh)
		m, err2 := strconv.Atoi(mm)
		return h, m, 1, err1 == nil && err2 == nil
	}
	h, n := number(w, 0)
	if n == 0 || w[0] == "a" || w[0] == "an" || w[0] == "couple" || w[0] == "few" {
		return 0, 0, 0, false
	}
	used = n
	if used < len(w) && w[used] == "oh" {
		if mm, n2 := number(w, used+1); n2 > 0 && mm < 10 {
			return h, mm, used + 1 + n2, true
		}
	}
	if used < len(w) && w[used] == "o'clock" {
		return h, 0, used + 1, true
	}
	if mm, n2 := number(w, used); n2 > 0 && mm >= 10 && mm < 60 {
		return h, mm, used + n2, true
	}
	return h, 0, used, true
}

// nextClock returns the next h:m in loc after now, or tomorrow's when asked.
func nextClock(h, m int, now time.Time, loc *time.Location, tomorrow bool) time.Time {
	ln := now.In(loc)
	t := time.Date(ln.Year(), ln.Month(), ln.Day(), h, m, 0, 0, loc)
	if tomorrow || !t.After(ln) {
		t = t.AddDate(0, 0, 1)
	}
	return t
}
