package providers

import (
	"context"
	"log/slog"

	"github.com/pod32g/omni-notify/internal/models"
)

// refBook looks up and remembers a provider's sent messages by ref, per
// channel, account and chat. Without a store, or for test sends, it remembers
// nothing and finds nothing.
type refBook struct {
	store                  MessageStore
	channel, account, chat string
}

func newRefBook(store MessageStore, msg models.NotificationMessage, channel, account, chat string) refBook {
	if store == nil || msg.Test || msg.EventRef <= 0 {
		return refBook{}
	}
	return refBook{store: store, channel: channel, account: account, chat: chat}
}

func (r refBook) get(ctx context.Context, ref string) (models.SentMessage, bool) {
	if r.store == nil || ref == "" {
		return models.SentMessage{}, false
	}
	m, err := r.store.GetSentMessage(ctx, r.channel, r.account, r.chat, ref)
	if err != nil || m.MessageID == "" {
		return models.SentMessage{}, false
	}
	return m, true
}

func (r refBook) put(ctx context.Context, ref, messageID, kind string) {
	if r.store == nil || ref == "" || messageID == "" {
		return
	}
	err := r.store.PutSentMessage(ctx, models.SentMessage{
		Channel: r.channel, Account: r.account, Chat: r.chat, Ref: ref, MessageID: messageID, Kind: kind,
	})
	if err != nil {
		slog.Warn(r.channel+": remember message", "ref", ref, "err", err)
	}
}

func (r refBook) forget(ctx context.Context, ref string) {
	if r.store == nil {
		return
	}
	if err := r.store.DeleteSentMessage(ctx, r.channel, r.account, r.chat, ref); err != nil {
		slog.Warn(r.channel+": forget message", "ref", ref, "err", err)
	}
}

// refOf is the ref a message is remembered by: the producer's, else the one it
// edits (it stands for that message now), else its event's fingerprint.
func refOf(explicit, edit string, ev models.Event) string {
	switch {
	case explicit != "":
		return explicit
	case edit != "":
		return edit
	}
	return ev.Fingerprint
}
