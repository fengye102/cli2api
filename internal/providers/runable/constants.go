package runable

import "time"

const (
	// ProviderID is the registry id and the model prefix that pins a route to
	// this provider.
	ProviderID = "runable"

	// Label is what the console shows for the channel.
	Label = "Runable"

	// CredentialFormat is the canonical stored credential format.
	CredentialFormat = "runable-cookie-v1"

	// BaseURL is the only host the web client talks to.
	BaseURL = "https://api.runable.com"

	// DeviceHeader is a constant the wall carries on every call. It is not a
	// session value — it identifies the web client build — so it lives here
	// rather than in the credential.
	DeviceHeader = "dweb/a69d4f72ddd711c660237adaf5c76287ac5473f5"

	// UserAgent matches the browser client closely enough to pass the edge.
	UserAgent = "Mozilla/5.0 (Linux; Android 14) RunableGateway/1.0"

	// ChatPath and ModelsPath are the two endpoints this adapter uses. The
	// model list is a POST with an empty body, not a GET.
	ChatPath   = "/api/chat/text"
	ModelsPath = "/api/rpc/public/models/getAIChatModels"
	// SessionPath is the web client's session probe. It is used for readiness
	// only: a chat call is the real proof, and the probe is cheap.
	SessionPath = "/api/auth/get-session"

	// CatalogTTL bounds how stale the model list may get. The vendor ships
	// models without notice and the list is small, so refreshing often is cheap.
	CatalogTTL = 10 * time.Minute

	// ProbeTimeout bounds the readiness check so a dead edge cannot hang the
	// console's test button.
	ProbeTimeout = 20 * time.Second
)
