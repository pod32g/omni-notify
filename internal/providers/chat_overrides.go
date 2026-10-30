package providers

import (
	"encoding/json"
	"errors"
)

// slackOverride is an event's message.slack, in incoming-webhook terms. Given
// text, blocks or attachments, they are the message (Block Kit and legacy
// attachments are passed through; Slack checks them); text is the fallback
// shown in notifications and defaults to message.text or the title.
type slackOverride struct {
	Text        string            `json:"text,omitempty"`
	Blocks      []json.RawMessage `json:"blocks,omitempty"`
	Attachments []json.RawMessage `json:"attachments,omitempty"`
	Mrkdwn      *bool             `json:"mrkdwn,omitempty"`
	UnfurlLinks *bool             `json:"unfurl_links,omitempty"`
	UnfurlMedia *bool             `json:"unfurl_media,omitempty"`
	ThreadTS    string            `json:"thread_ts,omitempty"`
	// Legacy webhooks only; app webhooks ignore them.
	Username  string `json:"username,omitempty"`
	IconEmoji string `json:"icon_emoji,omitempty"`
	IconURL   string `json:"icon_url,omitempty"`
}

func parseSlackOverride(raw json.RawMessage) (slackOverride, error) {
	var ov slackOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return slackOverride{}, err
	}
	if err := checkLen("text", ov.Text, 40000); err != nil {
		return slackOverride{}, err
	}
	if err := checkObjects("blocks", ov.Blocks, 50); err != nil {
		return slackOverride{}, err
	}
	if err := checkObjects("attachments", ov.Attachments, 100); err != nil {
		return slackOverride{}, err
	}
	return ov, checkWebURL("icon_url", ov.IconURL)
}

// teamsOverride is an event's message.teams: an Adaptive Card, sent the way a
// Teams Workflows webhook takes it, or a legacy MessageCard (Office 365
// connectors), passed through as given.
type teamsOverride struct {
	Card        json.RawMessage `json:"card,omitempty"`
	MessageCard json.RawMessage `json:"message_card,omitempty"`
}

func parseTeamsOverride(raw json.RawMessage) (teamsOverride, error) {
	var ov teamsOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return teamsOverride{}, err
	}
	if len(ov.Card) > 0 && len(ov.MessageCard) > 0 {
		return teamsOverride{}, errors.New("card and message_card can't be used together")
	}
	if err := checkObject("message_card", ov.MessageCard); err != nil {
		return teamsOverride{}, err
	}
	if len(ov.Card) > 0 {
		var card struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(ov.Card, &card) != nil || card.Type != "AdaptiveCard" {
			return teamsOverride{}, errors.New(`card must be an Adaptive Card object ("type": "AdaptiveCard")`)
		}
	}
	return ov, nil
}

// googleChatOverride is an event's message.googlechat, in Chat API terms:
// text (Chat's own formatting), cardsV2, and threadKey, which puts messages
// with the same key in one thread (starting it if needed).
type googleChatOverride struct {
	Text      string            `json:"text,omitempty"`
	CardsV2   []json.RawMessage `json:"cardsV2,omitempty"`
	ThreadKey string            `json:"threadKey,omitempty"`
}

func parseGoogleChatOverride(raw json.RawMessage) (googleChatOverride, error) {
	var ov googleChatOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return googleChatOverride{}, err
	}
	if err := checkLen("text", ov.Text, 4096); err != nil {
		return googleChatOverride{}, err
	}
	if err := checkObjects("cardsV2", ov.CardsV2, 0); err != nil {
		return googleChatOverride{}, err
	}
	return ov, checkLen("threadKey", ov.ThreadKey, maxRefLen)
}
