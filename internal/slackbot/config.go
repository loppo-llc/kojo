// Package slackbot provides Slack Socket Mode integration for Kojo agents.
package slackbot

import (
	"fmt"
	"time"
)

// Config holds Slack bot configuration for an agent.
type Config struct {
	Enabled       bool `json:"enabled"`
	ThreadReplies bool `json:"threadReplies"` // always reply in-thread (default true)
}

// Validate checks that the configuration is minimally valid.
func (c *Config) Validate() error {
	// Tokens are stored separately in CredentialStore; nothing to validate here
	// beyond the struct itself.
	return nil
}

// TokenProvider reads/writes encrypted Slack tokens from a credential store.
type TokenProvider interface {
	GetToken(provider, agentID, sourceID, key string) (string, error)
	SetToken(provider, agentID, sourceID, key, value string, expiresAt time.Time) error
	DeleteTokensBySource(provider, agentID, sourceID string) error
}

const (
	tokenProvider = "slack"
	tokenSourceID = "" // agent-level, no per-source scoping

	keyAppToken = "app_token"
	keyBotToken = "bot_token"
)

// StoreTokens saves the app and bot tokens to the credential store.
func StoreTokens(tp TokenProvider, agentID, appToken, botToken string) error {
	noExpiry := time.Time{}
	if err := tp.SetToken(tokenProvider, agentID, tokenSourceID, keyAppToken, appToken, noExpiry); err != nil {
		return fmt.Errorf("store app token: %w", err)
	}
	if err := tp.SetToken(tokenProvider, agentID, tokenSourceID, keyBotToken, botToken, noExpiry); err != nil {
		return fmt.Errorf("store bot token: %w", err)
	}
	return nil
}

// LoadTokens retrieves the app and bot tokens from the credential store.
func LoadTokens(tp TokenProvider, agentID string) (appToken, botToken string, err error) {
	appToken, err = tp.GetToken(tokenProvider, agentID, tokenSourceID, keyAppToken)
	if err != nil {
		return "", "", fmt.Errorf("load app token: %w", err)
	}
	botToken, err = tp.GetToken(tokenProvider, agentID, tokenSourceID, keyBotToken)
	if err != nil {
		return "", "", fmt.Errorf("load bot token: %w", err)
	}
	return appToken, botToken, nil
}

// DeleteTokens removes all Slack tokens for an agent.
func DeleteTokens(tp TokenProvider, agentID string) error {
	return tp.DeleteTokensBySource(tokenProvider, agentID, tokenSourceID)
}
