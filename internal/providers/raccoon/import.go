package raccoon

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The credential codec doubles as the importer. Raccoon has exactly one
// credential shape (an access token, optionally with the refresh token that
// mints the next access token), so "paste a token" and "paste the desktop
// client's auth.json" differ only in which JSON keys the decoder accepts.
//
// PrepareImport normalizes that input into the canonical payload and reports
// whether the account can serve a request immediately. A refresh-token-only
// payload is Ready: the first call mints an access token from it.
func (credentialCodec) Format() string { return CredentialFormat }

func (credentialCodec) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	if err := ValidateCredential(payload); err != nil {
		return providers.CredentialImport{}, err
	}
	credential, err := DecodeCredential(payload)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	encoded, err := EncodeCredential(credential)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	return providers.CredentialImport{Payload: encoded, Ready: credential.Ready()}, nil
}

type importer struct{}

func (importer) ValidateImport(payload []byte) error { return ValidateCredential(payload) }

// Export is unsupported: Raccoon credentials are pasted into this gateway only,
// never exported back to a third-party client.
func (importer) Export(context.Context, string) (map[string]any, error) {
	return nil, providers.ErrUnsupported
}
