package voice

import (
	"testing"
	"time"
)

var mexico, _ = time.LoadLocation("America/Mexico_City")

// 4:45 PM in Mexico City (UTC-6) on Monday 2026-10-26.
var now = time.Date(2026, 10, 26, 22, 45, 0, 0, time.UTC)

func TestParseIntentKinds(t *testing.T) {
	cases := map[string]IntentKind{
		"":                                     IntentSilence,
		"   ":                                  IntentSilence,
		"Okay.":                                IntentAcknowledge,
		"got it, thanks":                       IntentAcknowledge,
		"I'm up, I'm up":                       IntentAcknowledge,
		"Yeah":                                 IntentAcknowledge,
		"Can you repeat that?":                 IntentRepeat,
		"What?":                                IntentRepeat,
		"say that again":                       IntentRepeat,
		"What is this about?":                  IntentExplain,
		"tell me more":                         IntentExplain,
		"Who is this?":                         IntentIdentify,
		"dismiss":                              IntentDismiss,
		"never mind, forget it":                IntentDismiss,
		"don't remind me":                      IntentDismiss,
		"Bye":                                  IntentEnd,
		"no":                                   IntentEnd,
		"hang up":                              IntentEnd,
		"snooze":                               IntentSnooze,
		"later":                                IntentSnooze,
		"not now":                              IntentSnooze,
		"Give me another ten minutes.":         IntentSnooze,
		"no, give me ten more minutes":         IntentSnooze,
		"Call me back at 5:30 PM":              IntentReschedule,
		"call me tomorrow at nine":             IntentReschedule,
		"the purple elephant":                  IntentUnknown,
		"remind me again in half an hour":      IntentSnooze,
		"Give me 15 min":                       IntentSnooze,
		"call me back":                         IntentSnooze,
		"why are you calling":                  IntentExplain,
		"Okay, call me back in twenty minutes": IntentSnooze,
	}
	for in, want := range cases {
		if got := ParseIntent(in, now, mexico); got.Kind != want {
			t.Errorf("ParseIntent(%q) = %s, want %s", in, got.Kind, want)
		}
	}
}

func TestParseSnoozeLengths(t *testing.T) {
	cases := map[string]time.Duration{
		"Give me another ten minutes.":     10 * time.Minute,
		"ten more minutes":                 10 * time.Minute,
		"give me 10 more minutes":          10 * time.Minute,
		"five minutes":                     5 * time.Minute,
		"twenty five minutes":              25 * time.Minute,
		"twenty-five minutes":              25 * time.Minute,
		"in an hour":                       time.Hour,
		"a couple of hours":                2 * time.Hour,
		"half an hour":                     30 * time.Minute,
		"an hour and a half":               90 * time.Minute,
		"a few minutes":                    3 * time.Minute,
		"give me a minute":                 time.Minute,
		"snooze":                           DefaultSnooze,
		"thirty seconds":                   time.Minute, // never less than a minute
		"Okay, call me in 2 hours please.": 2 * time.Hour,
	}
	for in, want := range cases {
		got := ParseIntent(in, now, mexico)
		if got.Kind != IntentSnooze || got.After != want {
			t.Errorf("ParseIntent(%q) = %s %s, want snooze %s", in, got.Kind, got.After, want)
		}
	}
}

func TestParseRescheduleTimes(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, mexico) }
	cases := map[string]time.Time{
		"call me at 5":             at(26, 17, 0), // next 5 o'clock is today at 5 PM
		"call me at five thirty":   at(26, 17, 30),
		"at 5:30 pm":               at(26, 17, 30),
		"at 4":                     at(27, 4, 0), // 4 PM has passed: 4 AM tomorrow is next
		"at 4 pm":                  at(27, 16, 0),
		"at 9 am":                  at(27, 9, 0),
		"at nine in the morning":   at(27, 9, 0),
		"tomorrow at nine":         at(27, 9, 0),
		"tomorrow at 2":            at(27, 14, 0),
		"tomorrow at 7:15":         at(27, 7, 15),
		"at noon":                  at(27, 12, 0),
		"tomorrow at noon":         at(27, 12, 0),
		"at midnight":              at(27, 0, 0),
		"at 18:45":                 at(26, 18, 45),
		"call me at eight tonight": at(26, 20, 0),
		"at six oh five":           at(26, 18, 5),
		"at 5 o'clock":             at(26, 17, 0),
	}
	for in, want := range cases {
		got := ParseIntent(in, now, mexico)
		if got.Kind != IntentReschedule || !got.At.Equal(want) {
			t.Errorf("ParseIntent(%q) = %s %s, want reschedule %s", in, got.Kind, got.At.In(mexico), want)
		}
	}
}

func TestSpokenFollowsGobbie(t *testing.T) {
	cases := map[string]string{
		"Okay, I'll call you again in 10 minutes, at 5:42 PM.": "Okay, I'll call you again in ten minutes, at five forty-two P M.",
		"at 9 AM":                      "At nine A M",
		"at 6:05 PM":                   "At six oh five P M",
		"100% done":                    "One hundred percent done",
		"Lend me your phone — please…": "Lend me your phone, please.",
		"1205 builds":                  "One thousand two hundred and five builds",
	}
	for in, want := range cases {
		if got := Spoken(in); got != want {
			t.Errorf("Spoken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSayDuration(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Minute:  "ten minutes",
		time.Minute:       "one minute",
		time.Hour:         "an hour",
		90 * time.Minute:  "an hour and a half",
		150 * time.Minute: "two and a half hours",
		65 * time.Minute:  "an hour and five minutes",
		3 * time.Hour:     "three hours",
	}
	for d, want := range cases {
		if got := SayDuration(d); got != want {
			t.Errorf("SayDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestClockText(t *testing.T) {
	at := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, mexico) }
	cases := []struct {
		t    time.Time
		want string
	}{
		{at(26, 16, 55), "4:55 PM"},
		{at(26, 17, 0), "5 PM"},
		{at(27, 9, 0), "tomorrow at 9 AM"},
		{at(27, 12, 0), "tomorrow at noon"},
		{at(29, 8, 30), "Thursday at 8:30 AM"},
	}
	for _, c := range cases {
		if got := ClockText(c.t, now, mexico); got != c.want {
			t.Errorf("ClockText(%s) = %q, want %q", c.t, got, c.want)
		}
	}
}
