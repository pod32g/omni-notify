// Package dedupe computes stable event fingerprints and decides, per route,
// whether an event should trigger a notification or be suppressed as a duplicate.
package dedupe

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/pod32g/omni-notify/internal/models"
)

// Fingerprint returns the event's fingerprint, deriving a stable one from
// type, source, event_id and sorted labels when the producer did not supply it.
func Fingerprint(ev models.Event) string {
	if fp := strings.TrimSpace(ev.Fingerprint); fp != "" {
		return fp
	}
	h := sha256.New()
	writeField(h, "type", ev.Type)
	writeField(h, "source", ev.Source)
	writeField(h, "event_id", ev.EventID)

	keys := make([]string, 0, len(ev.Labels))
	for k := range ev.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		writeField(h, "label."+k, ev.Labels[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// writeField feeds a key/value pair into the hash with NUL separators so that
// different field boundaries cannot collide.
func writeField(h interface{ Write([]byte) (int, error) }, key, val string) {
	_, _ = h.Write([]byte(key))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(val))
	_, _ = h.Write([]byte{0})
}
