package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// discordOverride is an event's message.discord. Content fields follow the
// Execute Webhook API and are passed through as given; Discord checks them.
type discordOverride struct {
	Content         string            `json:"content,omitempty"`
	Embeds          []json.RawMessage `json:"embeds,omitempty"`
	Components      []json.RawMessage `json:"components,omitempty"` // sent with with_components=true
	Poll            json.RawMessage   `json:"poll,omitempty"`
	AllowedMentions json.RawMessage   `json:"allowed_mentions,omitempty"`
	Username        string            `json:"username,omitempty"`
	AvatarURL       string            `json:"avatar_url,omitempty"`
	TTS             bool              `json:"tts,omitempty"`
	Flags           []string          `json:"flags,omitempty"` // suppress_embeds, suppress_notifications, is_components_v2
	ThreadID        string            `json:"thread_id,omitempty"`
	ThreadName      string            `json:"thread_name,omitempty"`
	AppliedTags     []string          `json:"applied_tags,omitempty"`

	Ref    string `json:"ref,omitempty"`    // remember the message as
	Edit   string `json:"edit,omitempty"`   // edit this ref instead of sending
	Delete string `json:"delete,omitempty"` // delete this ref
	Send   *bool  `json:"send,omitempty"`   // false: only delete
}

// discordFlags are the message flags a webhook may set.
var discordFlags = map[string]int{
	"suppress_embeds":        1 << 2,
	"suppress_notifications": 1 << 12,
	"is_components_v2":       1 << 15,
}

var snowflake = regexp.MustCompile(`^[0-9]{1,20}$`)

func parseDiscordOverride(raw json.RawMessage) (discordOverride, error) {
	var ov discordOverride
	if err := decodeOverride(raw, &ov); err != nil {
		return discordOverride{}, err
	}
	return ov, ov.validate()
}

func (ov discordOverride) validate() error {
	if err := checkLen("content", ov.Content, 2000); err != nil {
		return err
	}
	if err := checkObjects("embeds", ov.Embeds, 10); err != nil {
		return err
	}
	if err := checkObjects("components", ov.Components, 0); err != nil {
		return err
	}
	for name, raw := range map[string]json.RawMessage{"poll": ov.Poll, "allowed_mentions": ov.AllowedMentions} {
		if err := checkObject(name, raw); err != nil {
			return err
		}
	}
	if err := checkLen("username", ov.Username, 80); err != nil {
		return err
	}
	if err := checkWebURL("avatar_url", ov.AvatarURL); err != nil {
		return err
	}
	v2 := false
	for _, f := range ov.Flags {
		if _, ok := discordFlags[f]; !ok {
			return fmt.Errorf("flags: %q is not suppress_embeds, suppress_notifications or is_components_v2", f)
		}
		v2 = v2 || f == "is_components_v2"
	}
	if v2 && (ov.Content != "" || len(ov.Embeds) > 0 || len(ov.Poll) > 0 || len(ov.Components) == 0) {
		return errors.New("is_components_v2 takes components only (no content, embeds or poll)")
	}
	if ov.ThreadID != "" && !snowflake.MatchString(ov.ThreadID) {
		return errors.New("thread_id must be a Discord ID")
	}
	for _, tag := range ov.AppliedTags {
		if !snowflake.MatchString(tag) {
			return errors.New("applied_tags must be Discord IDs")
		}
	}
	if err := checkLen("thread_name", ov.ThreadName, 100); err != nil {
		return err
	}
	if err := checkRefs(map[string]string{"ref": ov.Ref, "edit": ov.Edit, "delete": ov.Delete}); err != nil {
		return err
	}
	if ov.Send != nil && !*ov.Send && ov.Delete == "" {
		return errNothingToDo
	}
	return nil
}

// hasContent reports whether the producer gave the message's content.
func (ov discordOverride) hasContent() bool {
	return ov.Content != "" || len(ov.Embeds) > 0 || len(ov.Components) > 0 || len(ov.Poll) > 0
}

func discordFlagBits(names []string) int {
	bits := 0
	for _, n := range names {
		bits |= discordFlags[n]
	}
	return bits
}
