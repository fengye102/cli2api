package zcode

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// credentialCodec implements providers.CredentialCodec for the zcode
// format.
type credentialCodec struct{}

// Format returns the canonical credential format id.
func (credentialCodec) Format() string { return CredentialFormat }

// PrepareImport validates the pasted payload and returns the canonical
// bundle. Ready is true when a usable chat credential exists; identity
// fields are filled lazily by later probes.
func (credentialCodec) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	credential, err := DecodeCredential(payload)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	if err := ensureSupportedRegion(credential); err != nil {
		return providers.CredentialImport{}, err
	}
	encoded, err := credential.Encode()
	if err != nil {
		return providers.CredentialImport{}, err
	}
	return providers.CredentialImport{Payload: encoded, Ready: credential.Ready()}, nil
}

// Validate satisfies providers.CredentialCodec.
func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

// importer implements providers.ImportExporter for the console import
// wizard: a single credential becomes one account.
type importer struct{}

func (importer) ValidateImport(payload []byte) error { return ValidateCredential(payload) }

func (importer) Export(ctx context.Context, accountID string) (map[string]any, error) {
	return nil, providers.ErrUnsupported
}
