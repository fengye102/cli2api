package autoclaw

import "strings"

// Region is the AutoClaw build an account belongs to. It is a channel, not an
// account attribute: CN and Intl have different hosts and separate provider
// ids, so a given adapter instance only ever talks to one of them.
type Region int

const (
	// RegionCN is the domestic build (智谱加速域).
	RegionCN Region = iota
	// RegionIntl is the overseas build (autoglm.ai).
	RegionIntl
)

// String implements fmt.Stringer for logs and error messages.
func (r Region) String() string {
	if r == RegionIntl {
		return "intl"
	}
	return "cn"
}

// ProviderID returns the provider id this region is registered under.
func (r Region) ProviderID() string {
	if r == RegionIntl {
		return IntlProviderID
	}
	return ProviderID
}

// Label is the display suffix shown after "AutoClaw".
func (r Region) Label() string {
	if r == RegionIntl {
		return "国际版"
	}
	return "国内版"
}

// CredentialFormat returns the storage payload name for this region.
func (r Region) CredentialFormat() string {
	if r == RegionIntl {
		return IntlCredentialFormat
	}
	return CredentialFormat
}

// upstreamBase is the LLM proxy base (no trailing slash beyond /autoclaw).
func (r Region) upstreamBase() string {
	if r == RegionIntl {
		return intlUpstreamBase
	}
	return cnUpstreamBase
}

// userapiBase hosts refresh, balance and the model catalog.
func (r Region) userapiBase() string {
	if r == RegionIntl {
		return intlUserapiBase
	}
	return cnUserapiBase
}

// chatPath returns the region-specific chat path. The two builds genuinely
// differ (CN omits /v1), so this is not a cosmetic detail.
func (r Region) chatPath() string {
	if r == RegionIntl {
		return intlChatPath
	}
	return cnChatPath
}

// ResolveRegion maps a provider id to its region. Unknown ids yield CN and
// false so callers can reject them.
func ResolveRegion(providerID string) (Region, bool) {
	switch strings.TrimSpace(providerID) {
	case ProviderID:
		return RegionCN, true
	case IntlProviderID:
		return RegionIntl, true
	default:
		return RegionCN, false
	}
}

// ParseRegion accepts the console's region spelling ("cn-north-4" is the only
// region cli2api registers for CN; "global" for Intl) and falls back to CN.
func ParseRegion(value string) Region {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "global", "intl", "overseas":
		return RegionIntl
	default:
		return RegionCN
	}
}
