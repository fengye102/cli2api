package muse

import (
	"context"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// catalogModelInfos mirrors muse2api's public model registry: muse.ai's web
// client offers one assistant, one image model and one video model, and the
// gateway exposes exactly those three ids.
func catalogModelInfos() []providers.ModelInfo {
	return []providers.ModelInfo{
		{
			NativeModel: ModelChat,
			PublicModel: ModelChat,
			DisplayName: "Muse 助手",
			Capabilities: providers.ModelCapabilities{
				// muse.ai's agent accepts images in the prompt, has no tool
				// calling and no reasoning switch.
				Images: true,
				Tools:  false,
			},
		},
		{
			NativeModel: ModelImage,
			PublicModel: ModelImage,
			DisplayName: "Muse 生图",
			Capabilities: providers.ModelCapabilities{
				Images: true,
			},
		},
		{
			NativeModel: ModelVideo,
			PublicModel: ModelVideo,
			DisplayName: "Muse 生视频",
			Capabilities: providers.ModelCapabilities{
				Images: true,
			},
		},
	}
}

// importer validates and normalises pasted credentials for the console.
type importer struct{}

func (importer) Format() string { return CredentialFormat }

// PrepareImport normalises any supported paste shape into muse-cookie-v1.
func (i importer) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	credential, err := ParseImport(payload)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	if err := RequireSessionCookies(credential); err != nil {
		return providers.CredentialImport{}, err
	}
	encoded, err := credential.EncodeForImport()
	if err != nil {
		return providers.CredentialImport{}, err
	}
	return providers.CredentialImport{Payload: encoded, Ready: true}, nil
}

// ValidateImport satisfies ImportExporter.
func (i importer) ValidateImport(payload []byte) error { return ValidateCredential(payload) }

// Export reports the cookie names and masks their values: a muse.ai session
// cookie is a live credential, so an export must never leak it into a
// screenshot or a support thread.
func (i importer) Export(ctx context.Context, accountID string) (map[string]any, error) {
	_ = ctx
	_ = accountID
	return nil, providers.ErrUnsupported
}

// Ensure the optional interfaces are actually satisfied.
var (
	_ providers.CredentialCodec    = credentialCodec{}
	_ providers.CredentialImporter = credentialCodec{}
	_ providers.ImportExporter     = importer{}
	_ ChatTransport                = (*HTTPTransport)(nil)
)
