package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// pagerDutyOverride is an event's message.pagerduty, in Events API v2 terms:
// the alert's payload fields and the event's links and images.
type pagerDutyOverride struct {
	Summary       string          `json:"summary,omitempty"`
	Severity      string          `json:"severity,omitempty"` // critical, error, warning or info
	Component     string          `json:"component,omitempty"`
	Group         string          `json:"group,omitempty"`
	Class         string          `json:"class,omitempty"`
	CustomDetails json.RawMessage `json:"custom_details,omitempty"`
	Links         []pdLink        `json:"links,omitempty"`
	Images        []pdImage       `json:"images,omitempty"`
	Client        string          `json:"client,omitempty"`
	ClientURL     string          `json:"client_url,omitempty"`
	DedupKey      string          `json:"dedup_key,omitempty"`    // default: the event's fingerprint
	EventAction   string          `json:"event_action,omitempty"` // trigger, acknowledge or resolve; default from status
}

type pdLink struct {
	Href string `json:"href"`
	Text string `json:"text,omitempty"`
}

type pdImage struct {
	Src  string `json:"src"`
	Href string `json:"href,omitempty"`
	Alt  string `json:"alt,omitempty"`
}

var pdSeverities = map[string]bool{"critical": true, "error": true, "warning": true, "info": true}

func parsePagerDutyOverride(raw json.RawMessage) (pagerDutyOverride, error) {
	var ov pagerDutyOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return pagerDutyOverride{}, err
	}
	if err := checkLen("summary", ov.Summary, 1024); err != nil {
		return pagerDutyOverride{}, err
	}
	if ov.Severity != "" && !pdSeverities[ov.Severity] {
		return pagerDutyOverride{}, errors.New("severity must be critical, error, warning or info")
	}
	switch ov.EventAction {
	case "", "trigger", "acknowledge", "resolve":
	default:
		return pagerDutyOverride{}, errors.New("event_action must be trigger, acknowledge or resolve")
	}
	if err := checkObject("custom_details", ov.CustomDetails); err != nil {
		return pagerDutyOverride{}, err
	}
	for i, l := range ov.Links {
		if err := checkWebURL(fmt.Sprintf("links[%d].href", i), l.Href); err != nil || l.Href == "" {
			return pagerDutyOverride{}, fmt.Errorf("links[%d].href must be an http(s) URL", i)
		}
	}
	for i, im := range ov.Images {
		if !isWebURL(im.Src) {
			return pagerDutyOverride{}, fmt.Errorf("images[%d].src must be an http(s) URL", i)
		}
	}
	if err := checkLen("dedup_key", ov.DedupKey, 255); err != nil {
		return pagerDutyOverride{}, err
	}
	return ov, checkWebURL("client_url", ov.ClientURL)
}

// opsgenieOverride is an event's message.opsgenie, in Alert API terms (create
// alert); note also goes with closing one.
type opsgenieOverride struct {
	Message     string            `json:"message,omitempty"`
	Description string            `json:"description,omitempty"`
	Priority    string            `json:"priority,omitempty"` // P1 to P5
	Tags        []string          `json:"tags,omitempty"`
	Details     map[string]string `json:"details,omitempty"`
	Actions     []string          `json:"actions,omitempty"`
	Entity      string            `json:"entity,omitempty"`
	Note        string            `json:"note,omitempty"`
	User        string            `json:"user,omitempty"`
	Responders  []json.RawMessage `json:"responders,omitempty"`
	VisibleTo   []json.RawMessage `json:"visibleTo,omitempty"`
	Alias       string            `json:"alias,omitempty"` // default: the event's fingerprint
}

var opsgeniePriority = regexp.MustCompile(`^P[1-5]$`)

func parseOpsgenieOverride(raw json.RawMessage) (opsgenieOverride, error) {
	var ov opsgenieOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return opsgenieOverride{}, err
	}
	for name, l := range map[string][2]any{"message": {ov.Message, 130}, "description": {ov.Description, 15000},
		"entity": {ov.Entity, 512}, "note": {ov.Note, 25000}, "alias": {ov.Alias, 512}, "user": {ov.User, 100}} {
		if err := checkLen(name, l[0].(string), l[1].(int)); err != nil {
			return opsgenieOverride{}, err
		}
	}
	if ov.Priority != "" && !opsgeniePriority.MatchString(ov.Priority) {
		return opsgenieOverride{}, errors.New("priority must be P1 to P5")
	}
	if len(ov.Tags) > 20 || len(ov.Actions) > 10 {
		return opsgenieOverride{}, errors.New("at most 20 tags and 10 actions")
	}
	if err := checkObjects("responders", ov.Responders, 50); err != nil {
		return opsgenieOverride{}, err
	}
	return ov, checkObjects("visibleTo", ov.VisibleTo, 50)
}

// twilioOverride is an event's message.twilio: the SMS body, and media URLs
// that make it an MMS (Twilio fetches them).
type twilioOverride struct {
	Body     string   `json:"body,omitempty"`
	MediaURL []string `json:"media_url,omitempty"`
}

func parseTwilioOverride(raw json.RawMessage) (twilioOverride, error) {
	var ov twilioOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return twilioOverride{}, err
	}
	if err := checkLen("body", ov.Body, 1600); err != nil {
		return twilioOverride{}, err
	}
	if len(ov.MediaURL) > 10 {
		return twilioOverride{}, errors.New("media_url: at most 10")
	}
	for i, u := range ov.MediaURL {
		if !isWebURL(u) || !strings.HasPrefix(u, "http") {
			return twilioOverride{}, fmt.Errorf("media_url[%d] must be an http(s) URL", i)
		}
	}
	return ov, nil
}
