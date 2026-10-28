package models

// NotificationMessage is the provider-facing view of an event to be delivered.
// Providers format this however suits their channel; it carries enough of the
// event for rich rendering without exposing storage internals.
type NotificationMessage struct {
	// Event is the full event being delivered.
	Event Event
	// Route is the name of the route that selected this delivery.
	Route string
	// Test indicates a synthetic message sent via POST /api/v1/test.
	Test bool
	// EventRef and DeliveryID identify the stored event and delivery (zero for
	// test sends). Providers that act on the event later (Telegram callback
	// buttons) key on them.
	EventRef   int64
	DeliveryID int64
}
