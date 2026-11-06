package custom

import "time"

const (
	// CredentialFormat is the canonical stored credential format.
	CredentialFormat = "custom-relay-v1"

	// ProviderID is the registry id and the model prefix that pins a route to
	// this provider.
	ProviderID = "custom"

	// DefaultBasePath is appended when the pasted base URL has no path of its
	// own: relay operators publish either "https://host/v1" or the bare origin,
	// and the second form needs the version segment added.
	DefaultBasePath = "/v1"

	// ChatPath and ModelsPath are relative to the normalised base URL.
	ChatPath   = "/chat/completions"
	ModelsPath = "/models"

	// CatalogTTL bounds how stale a relay's model list may get. Relays add and
	// retire models without notice, so this is shorter than the 30m the
	// first-party providers use.
	CatalogTTL = 5 * time.Minute

	// ProbeTimeout bounds the readiness check so a dead relay cannot hang the
	// console's "test" button.
	ProbeTimeout = 20 * time.Second
)
