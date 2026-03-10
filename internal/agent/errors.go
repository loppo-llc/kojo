package agent

import "errors"

// Sentinel errors for the agent package.
var (
	ErrAgentNotFound = errors.New("agent not found")
	ErrAgentBusy     = errors.New("agent is busy")
	ErrAgentResetting = errors.New("agent is being reset")

	ErrGroupNotFound    = errors.New("group not found")
	ErrGroupNotMember   = errors.New("agent is not a member of group")
	ErrGroupTooFew      = errors.New("group requires at least 2 members")

	ErrCredentialNotFound = errors.New("credential not found")
	ErrNoTOTPSecret       = errors.New("no TOTP secret configured")

	ErrUnsupportedTool     = errors.New("unsupported tool")
	ErrUnsupportedInterval = errors.New("unsupported interval")
)
