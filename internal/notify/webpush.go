package notify

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	webpush "github.com/SherClockHolmes/webpush-go"
)

const configDir = ".config/kojo"
const vapidFile = "vapid.json"

type Manager struct {
	mu            sync.Mutex
	logger        *slog.Logger
	vapidPrivate  string
	vapidPublic   string
	subscriptions []*webpush.Subscription
}

type vapidKeys struct {
	PrivateKey string `json:"privateKey"`
	PublicKey  string `json:"publicKey"`
}

func NewManager(logger *slog.Logger) (*Manager, error) {
	m := &Manager{
		logger:        logger,
		subscriptions: make([]*webpush.Subscription, 0),
	}
	if err := m.loadOrGenerateVAPID(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) VAPIDPublicKey() string {
	return m.vapidPublic
}

func (m *Manager) Subscribe(sub *webpush.Subscription) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// dedupe by endpoint
	for _, existing := range m.subscriptions {
		if existing.Endpoint == sub.Endpoint {
			return
		}
	}
	m.subscriptions = append(m.subscriptions, sub)
	ep := sub.Endpoint
	if len(ep) > 50 {
		ep = ep[:50] + "..."
	}
	m.logger.Info("push subscription added", "endpoint", ep)
}

func (m *Manager) Unsubscribe(endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, sub := range m.subscriptions {
		if sub.Endpoint == endpoint {
			m.subscriptions = append(m.subscriptions[:i], m.subscriptions[i+1:]...)
			return
		}
	}
}

func (m *Manager) Send(payload []byte) {
	m.mu.Lock()
	subs := make([]*webpush.Subscription, len(m.subscriptions))
	copy(subs, m.subscriptions)
	m.mu.Unlock()

	for _, sub := range subs {
		resp, err := webpush.SendNotification(payload, sub, &webpush.Options{
			VAPIDPublicKey:  m.vapidPublic,
			VAPIDPrivateKey: m.vapidPrivate,
			Subscriber:      "mailto:kojo@localhost",
		})
		if err != nil {
			m.logger.Debug("push send failed", "err", err)
			continue
		}
		resp.Body.Close()
	}
}

func (m *Manager) loadOrGenerateVAPID() error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, configDir)
	path := filepath.Join(dir, vapidFile)

	data, err := os.ReadFile(path)
	if err == nil {
		var keys vapidKeys
		if err := json.Unmarshal(data, &keys); err == nil && keys.PrivateKey != "" {
			m.vapidPrivate = keys.PrivateKey
			m.vapidPublic = keys.PublicKey
			m.logger.Info("loaded VAPID keys")
			return nil
		}
	}

	// generate new keys
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate VAPID key: %w", err)
	}

	privBytes, err := x509.MarshalECPrivateKey(privKey)
	if err != nil {
		return fmt.Errorf("failed to marshal private key: %w", err)
	}

	pubBytes := elliptic.Marshal(elliptic.P256(), privKey.PublicKey.X, privKey.PublicKey.Y)

	m.vapidPrivate = base64.RawURLEncoding.EncodeToString(privBytes)
	m.vapidPublic = base64.RawURLEncoding.EncodeToString(pubBytes)

	// save
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("failed to create config dir: %w", err)
	}

	keys := vapidKeys{
		PrivateKey: m.vapidPrivate,
		PublicKey:  m.vapidPublic,
	}
	data, _ = json.MarshalIndent(keys, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("failed to save VAPID keys: %w", err)
	}

	m.logger.Info("generated new VAPID keys")
	return nil
}
