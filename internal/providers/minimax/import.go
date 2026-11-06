package minimax

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

func (credentialCodec) Format() string { return CredentialFormat }

// PrepareImport normalises a pasted credential into the canonical payload.
//
// Three pastes must work, because all three are things an operator actually has:
// the adapter's own 凭证 JSON, a bare access token (what the console's paste box
// produces), and minimaxcode2api's tokens.json. Whatever the shape, the stored
// payload is the canonical one, so the rest of the adapter never sees the alias
// spellings again.
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

type importer struct{}

func (importer) ValidateImport(payload []byte) error { return ValidateCredential(payload) }

// Export stays unsupported: a lived credential is a rotating OAuth pair, and a
// console export would hand out a refresh token that the first use invalidates.
func (importer) Export(context.Context, string) (map[string]any, error) {
	return nil, providers.ErrUnsupported
}
