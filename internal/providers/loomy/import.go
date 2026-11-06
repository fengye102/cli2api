package loomy

import (
	"context"
	"encoding/json"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Import. Three shapes are worth accepting, because all three are things an
// operator actually has on disk:
//
//	{"loginid":"138…","password":"…"}   one account, renewable
//	{"session":"…"}                     one live session, not renewable
//	{"accounts":[{…},{…}]}              loomy2api's accounts.json, split into
//	                                    one import per account
func (credentialCodec) Format() string { return CredentialFormat }

func (credentialCodec) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	if err := ValidateCredential(payload); err != nil {
		return providers.CredentialImport{}, err
	}
	cred, err := DecodeCredential(payload)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	encoded, err := EncodeCredential(cred)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	return providers.CredentialImport{Payload: encoded, Ready: cred.Ready()}, nil
}

// SplitImport unwraps a multi-account package. loomy2api writes
// {"accounts":[…]} and a hand-rolled export is often a bare array.
func (credentialCodec) SplitImport(payload []byte) ([][]byte, error) {
	var wrapper struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(payload, &wrapper); err == nil && len(wrapper.Accounts) > 0 {
		return toPayloads(wrapper.Accounts), nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(payload, &list); err == nil && len(list) > 0 {
		return toPayloads(list), nil
	}
	return nil, providers.ErrUnsupported
}

func toPayloads(raw []json.RawMessage) [][]byte {
	out := make([][]byte, 0, len(raw))
	for _, entry := range raw {
		out = append(out, []byte(entry))
	}
	return out
}

type importer struct{}

func (importer) ValidateImport(payload []byte) error { return ValidateCredential(payload) }

// Export is unsupported: a Loomy session is this gateway's own login, not
// something to hand back to a third-party client.
func (importer) Export(context.Context, string) (map[string]any, error) {
	return nil, providers.ErrUnsupported
}
