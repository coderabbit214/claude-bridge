package platform

import "context"

// Message is the normalized inbound message shape used by the bridge core.
type Message struct {
	UserID string
	Text   string
}

// Adapter hides platform-specific polling, reply, and typing behavior.
type Adapter interface {
	Events() <-chan Message
	Run(ctx context.Context) error
	Ready() <-chan struct{}
	SendText(ctx context.Context, userID, text string) error
	SetTyping(ctx context.Context, userID string, on bool) error
	Close() error
}
