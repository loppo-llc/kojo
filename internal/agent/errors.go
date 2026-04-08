package agent

import "errors"

// Sentinel errors for the agent package.
var (
	ErrAgentNotFound = errors.New("agent not found")
	ErrAgentBusy     = errors.New("agent is busy")
	ErrAgentResetting = errors.New("agent is being reset")

	ErrGroupNotFound      = errors.New("group not found")
	ErrGroupNotMember     = errors.New("agent is not a member of group")
	ErrGroupTooFew        = errors.New("group requires at least 2 members")
	ErrGroupAlreadyMember = errors.New("agent is already a member of group")

	ErrCredentialNotFound = errors.New("credential not found")
	ErrNoTOTPSecret       = errors.New("no TOTP secret configured")
	ErrInvalidTOTP        = errors.New("invalid TOTP parameters")

	ErrUnsupportedTool     = errors.New("unsupported tool")
	ErrUnsupportedInterval = errors.New("unsupported interval")
	ErrUnsupportedTimeout  = errors.New("unsupported timeout")
)
