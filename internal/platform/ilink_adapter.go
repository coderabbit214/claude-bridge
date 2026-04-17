package platform

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"time"
	"unsafe"

	wechatbot "github.com/corespeed-io/wechatbot/golang"
)

const maxChunkRunes = 4000

// ILinkAdapter adapts the wechatbot SDK to the generic platform interface.
type ILinkAdapter struct {
	bot           *wechatbot.Bot
	events        chan Message
	ready         chan struct{}
	readyOnce     sync.Once
	contextPath   string
	contextMu     sync.Mutex
	contextTokens map[string]string
}

// NewILinkAdapter creates an adapter backed by the wechatbot SDK.
// credPath is the file path for credential persistence.
func NewILinkAdapter(credPath string) *ILinkAdapter {
	bot := wechatbot.New(wechatbot.Options{
		CredPath: credPath,
		LogLevel: "warn",
	})
	stateDir := filepath.Dir(credPath)
	return &ILinkAdapter{
		bot:           bot,
		events:        make(chan Message, 64),
		ready:         make(chan struct{}),
		contextPath:   ContextTokensPath(stateDir),
		contextTokens: LoadContextTokens(ContextTokensPath(stateDir)),
	}
}

func (a *ILinkAdapter) Events() <-chan Message {
	return a.events
}

func (a *ILinkAdapter) Ready() <-chan struct{} {
	return a.ready
}

func (a *ILinkAdapter) Run(ctx context.Context) error {
	// Load credentials from disk (force=false skips QR flow if already logged in).
	if _, err := a.bot.Login(ctx, false); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	a.restoreContextTokens()
	a.readyOnce.Do(func() { close(a.ready) })
	a.bot.OnMessage(func(msg *wechatbot.IncomingMessage) {
		a.saveContextToken(msg.UserID, msg.ContextToken)
		if msg.Text == "" {
			return
		}
		select {
		case a.events <- Message{UserID: msg.UserID, Text: msg.Text}:
		case <-ctx.Done():
		}
	})
	return a.bot.Run(ctx)
}

// SendText splits long text at ≤4000-rune boundaries and sends each chunk.
func (a *ILinkAdapter) SendText(ctx context.Context, userID, text string) error {
	runes := []rune(text)
	for len(runes) > 0 {
		chunk := runes
		if len(chunk) > maxChunkRunes {
			cut := maxChunkRunes
			for cut > 0 && runes[cut] != '\n' {
				cut--
			}
			if cut == 0 {
				cut = maxChunkRunes
			}
			chunk = runes[:cut]
			runes = runes[cut:]
		} else {
			runes = nil
		}
		if err := a.bot.Send(ctx, userID, string(chunk)); err != nil {
			return err
		}
		if len(runes) > 0 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return nil
}

func (a *ILinkAdapter) SetTyping(ctx context.Context, userID string, on bool) error {
	if on {
		return a.bot.SendTyping(ctx, userID)
	}
	return a.bot.StopTyping(ctx, userID)
}

func (a *ILinkAdapter) Close() error {
	a.bot.Stop()
	return nil
}

func (a *ILinkAdapter) saveContextToken(userID, contextToken string) {
	if userID == "" || contextToken == "" {
		return
	}
	a.contextMu.Lock()
	defer a.contextMu.Unlock()
	if a.contextTokens[userID] == contextToken {
		return
	}
	a.contextTokens[userID] = contextToken
	_ = SaveContextTokens(a.contextTokens, a.contextPath)
}

func (a *ILinkAdapter) restoreContextTokens() {
	a.contextMu.Lock()
	defer a.contextMu.Unlock()
	for userID, token := range a.contextTokens {
		setBotContextToken(a.bot, userID, token)
	}
}

func setBotContextToken(bot *wechatbot.Bot, userID, contextToken string) {
	botValue := reflect.ValueOf(bot).Elem()
	field := botValue.FieldByName("contextTokens")
	if !field.IsValid() {
		return
	}
	ptr := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	method := ptr.Addr().MethodByName("Store")
	if !method.IsValid() {
		return
	}
	method.Call([]reflect.Value{reflect.ValueOf(userID), reflect.ValueOf(contextToken)})
}
