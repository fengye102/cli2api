package antigravity

// Pinned upstream constants for Google Antigravity. The adapter authenticates
// with a Gemini API key and speaks the official Gemini OpenAI-compatibility
// layer; no account session or OAuth token is involved.
const (
	// CredentialFormat is the canonical stored credential format for
	// provider=antigravity.
	CredentialFormat = "antigravity-key-v1"

	// BaseURL is the OpenAI-compatibility origin, already including the
	// v1beta/openai version segment. Chat and models live under it.
	BaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

	// PathModels is the OpenAI-compatible model list. Gemini's OpenAI-compat
	// layer honors the standard data[].id shape for gemini-* models.
	PathModels = "/models"

	// PathChat is the OpenAI-compatible chat completions endpoint.
	PathChat = "/chat/completions"

	// GeminiPrefix is the model id prefix that accepts the OpenAI
	// reasoning_effort field. Claude and GPT-OSS models served by Antigravity
	// keep their upstream thinking behavior and must not receive an effort
	// value.
	GeminiPrefix = "gemini-"

	// DefaultMaxTokens caps output when the client sends no max_tokens. The
	// gateway clamps anything larger to the model's true ceiling.
	DefaultMaxTokens = 65536

	// QuotaUnit labels the token-denominated quota windows.
	QuotaUnit = "percent"
)
