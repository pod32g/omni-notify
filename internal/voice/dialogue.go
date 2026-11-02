package voice

import (
	"context"
	"strings"
	"time"

	"github.com/pod32g/omni-notify/internal/models"
)

// Utterance is something the phone says: the display text and the form the
// voice reads (numbers and times in words).
type Utterance struct {
	Text   string `json:"text"`
	Speech string `json:"speech"`
}

func say(parts ...string) Utterance {
	text := strings.Join(nonEmpty(parts), " ")
	return Utterance{Text: text, Speech: Spoken(text)}
}

func nonEmpty(parts []string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ActionResult tells the responder what the service did with an intent's
// action, so the reply never claims more than what was persisted.
type ActionResult struct {
	// Done is true once the action is committed to storage.
	Done bool
	// Refused explains, in words for the caller, why it was not done.
	Refused string
	// At is when the follow-up call will ring (snooze/reschedule).
	At time.Time
	// AfterQuiet is set when At was moved to the end of quiet hours.
	AfterQuiet bool
}

// Responder is the pluggable conversational backend. It only interprets and
// phrases: every action is validated and persisted by the Service, never by
// the responder, so a language-model responder can replace the scripted one
// without being trusted with anything.
type Responder interface {
	// Greeting is what the phone says first: who is calling, for whom. The
	// phone then pauses (GreetingPause) so the recipient can switch to the
	// speaker before the message starts.
	Greeting(c models.Call) Utterance
	// Opening follows the greeting: the message itself.
	Opening(c models.Call) Utterance
	// Interpret maps what the recipient said to an intent.
	Interpret(ctx context.Context, c models.Call, transcript string, now time.Time, loc *time.Location) (Intent, error)
	// Reply phrases the answer to an intent once its action (if any) has been
	// applied. misses counts earlier turns that were not understood or silent.
	// It returns whether the call should end after the reply.
	Reply(c models.Call, in Intent, res ActionResult, misses int, now time.Time, loc *time.Location) (Utterance, bool)
}

// Scripted is the deterministic responder: keyword intents and fixed phrasing.
type Scripted struct{}

const prompt = "Say okay, or tell me when to call back."

// GreetingPause is the longest the phone waits after the greeting, held to
// the ear, for the recipient to switch to the speaker. It goes on at once when
// they do, and does not wait at all when the call is already on a speaker or
// headset.
const GreetingPause = 3 * time.Second

// voiceName is who speaks on the call: Gobbie, whose on-device voice the
// phone uses (and who calls herself that in OmniWeather).
const voiceName = "Gobbie"

// Greeting introduces who is speaking and for whom. Gobbie reads the message
// for the agent: she never pretends the originating agent is on the line.
func (Scripted) Greeting(c models.Call) Utterance {
	switch {
	case c.Test:
		return say("Hi, this is " + voiceName + " with a test call from Omni Notify, for " + c.CallerName + ".")
	case c.Simulated:
		return say("Hi, this is " + voiceName + " with a simulated call for " + c.CallerName + ".")
	case c.Origin == OriginPhone:
		return say("Hi, it's " + voiceName + ", calling you back for " + c.CallerName + ".")
	case strings.EqualFold(c.Type, "reminder"):
		return say("Hi, this is " + voiceName + " with a reminder from " + c.CallerName + ".")
	case c.Severity == models.SeverityCritical:
		return say("This is " + voiceName + " with a critical alert from " + c.CallerName + ".")
	default:
		return say("Hi, this is " + voiceName + " with a message from " + c.CallerName + ".")
	}
}

// identify answers "who is this?".
func identify(c models.Call) []string {
	return []string{"This is " + voiceName + ", from Omni Notify, with a message from " + c.CallerName + ".",
		"I'm reading it for them; " + c.CallerName + " isn't on the line."}
}

// Opening reads the message after the greeting.
func (Scripted) Opening(c models.Call) Utterance {
	if !c.Interactive {
		return say(c.Message, "That's all. Goodbye.")
	}
	return say(c.Message, prompt)
}

// Interpret uses the keyword parser.
func (Scripted) Interpret(_ context.Context, _ models.Call, transcript string, now time.Time, loc *time.Location) (Intent, error) {
	return ParseIntent(transcript, now, loc), nil
}

// Reply phrases the outcome of an intent.
func (Scripted) Reply(c models.Call, in Intent, res ActionResult, misses int, now time.Time, loc *time.Location) (Utterance, bool) {
	if res.Refused != "" {
		return say(res.Refused, prompt), false
	}
	switch in.Kind {
	case IntentRepeat:
		return say(c.Message, prompt), false
	case IntentIdentify:
		return say(append(identify(c), prompt)...), false
	case IntentExplain:
		if c.Details != "" {
			return say("Here's what else "+c.CallerName+" said.", c.Details, prompt), false
		}
		return say(c.CallerName+" didn't send any more detail than that.", prompt), false
	case IntentAcknowledge:
		return say("Got it.", "Bye."), true
	case IntentDismiss:
		return say("Okay, dismissed.", "Bye."), true
	case IntentSnooze, IntentReschedule:
		// Replies open with one of Phrases: the phone has it rendered already,
		// so it is heard at once and the specific sentence renders meanwhile.
		when := ClockText(res.At, now, loc)
		if res.AfterQuiet {
			return say("Okay, no problem.", "That's in your quiet hours, so I'll call you at "+when+" instead.", "Bye."), true
		}
		if in.Kind == IntentSnooze {
			return say("Okay, no problem.", "I'll call you again in "+SayDuration(res.At.Sub(now))+", at "+when+".", "Bye."), true
		}
		return say("Okay, no problem.", "I'll call you back "+atPhrase(when)+".", "Bye."), true
	case IntentEnd:
		return say("Okay.", "Bye."), true
	case IntentSilence:
		if misses >= 1 {
			return say(leaveIt, "Bye."), true
		}
		return say("Are you there?", prompt), false
	default:
		if misses >= 2 {
			return say(leaveIt, "Bye."), true
		}
		return say(notCaught, example), false
	}
}

const (
	leaveIt   = "I'll leave it there."
	notCaught = "Sorry, I didn't catch that."
	example   = "Say okay, or tell me when to call back, like in ten minutes."
)

// Phrases are the fixed sentences replies to this call are built from, in
// spoken form. The phone renders them while the call rings, so most replies
// start without waiting for the voice.
func (Scripted) Phrases(c models.Call) []string {
	// In the order they are likely to be needed: the phone renders them in
	// this order while the call rings and while the opening plays.
	more := c.CallerName + " didn't send any more detail than that."
	if c.Details != "" {
		more = "Here's what else " + c.CallerName + " said."
	}
	p := append([]string{"Okay, no problem.", "Got it.", "Bye.", notCaught, more, "Okay, dismissed.", "Okay.",
		"Are you there?", prompt, leaveIt, example}, identify(c)...)
	for i := range p {
		p[i] = Spoken(p[i])
	}
	return p
}

// Phraser is implemented by responders whose replies reuse fixed sentences.
type Phraser interface {
	Phrases(c models.Call) []string
}

// atPhrase turns ClockText into "at 5 PM" or "tomorrow at 9 AM".
func atPhrase(when string) string {
	if strings.Contains(when, " at ") {
		return when
	}
	return "at " + when
}
