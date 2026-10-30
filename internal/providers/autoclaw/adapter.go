package autoclaw

import (
	"context"
	"fmt"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Adapter wires the capability surface for this region.
//
// There is no browser login: the desktop client's OAuth round needs a real
// desktop environment (a loopback listener on one of four whitelisted ports
// plus an Aliyun captcha in the browser), which a server cannot provide. Accounts
// are added by pasting the desktop credential JSON, so Login stays nil.
func (c *Client) Adapter() providers.Adapter {
	return providers.Adapter{
		ID:           c.region.ProviderID(),
		Credential:   credentialCodec{},
		Chat:         c,
		Models:       c,
		Classifier:   classifier{},
		ImportExport: importer{region: c.region},
		Prober:       c,
	}
}

// Probe verifies the credential by reaching the model catalog.
//
// A catalog failure is reported as not-ready, but a quota failure is not: an
// account out of credits can still be probed, and conflating the two would
// take healthy accounts offline.
func (c *Client) Probe(ctx context.Context, accountID string) (providers.AccountHealth, error) {
	cred, err := c.credential(ctx, accountID)
	if err != nil {
		return providers.AccountHealth{LastError: err.Error()}, nil
	}
	if _, err := c.fetchRemoteCatalog(ctx, accountID); err != nil {
		message := err.Error()
		if providerErr, ok := err.(*providers.Error); ok {
			message = providerErr.Message
		}
		_ = c.store.Observe(ctx, accountID, cred.UserID, "error", message, "")
		return providers.AccountHealth{LastError: message}, nil
	}
	uid := cred.UserID
	if uid == "" {
		uid = jwtUserID(cred.AccessToken)
	}
	_ = c.store.Observe(ctx, accountID, uid, "ready", "", "")
	return providers.AccountHealth{Ready: true, Hot: true, UID: uid}, nil
}

type credentialCodec struct{}

func (credentialCodec) Validate(payload []byte) error { return ValidateCredential(payload) }

// importer validates and exports credentials for the console.
type importer struct {
	region Region
}

func (i importer) Format() string { return i.region.CredentialFormat() }

// PrepareImport normalizes a pasted credential into the canonical payload.
//
// Accepts the desktop auth.json shape as well as a bare token. A credential
// with only an access token is importable but not refreshable, which the
// console shows as a permanent token rather than a silent failure.
func (i importer) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	cred, err := DecodeCredential(payload)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	if !cred.Ready() {
		return providers.CredentialImport{}, fmt.Errorf("autoclaw credential needs an access token or a refresh token")
	}
	cred.Format = i.region.CredentialFormat()
	cred.Region = i.region.String()
	if cred.UserID == "" {
		cred.UserID = jwtUserID(cred.AccessToken)
	}
	encoded, err := EncodeCredential(cred)
	if err != nil {
		return providers.CredentialImport{}, err
	}
	return providers.CredentialImport{Payload: encoded, Ready: true}, nil
}

// ValidateImport satisfies ImportExporter.
func (i importer) ValidateImport(payload []byte) error { return ValidateCredential(payload) }

// Export returns the stored credential as JSON for the console's export
// button. AutoClaw credentials carry live tokens, so the export masks them and
// reports only the shape - a full export would let a screenshot leak a session.
func (i importer) Export(ctx context.Context, accountID string) (map[string]any, error) {
	return nil, providers.ErrUnsupported
}

// Ensure the optional interfaces are actually satisfied.
var (
	_ providers.CredentialCodec      = credentialCodec{}
	_ providers.ProviderChat         = (*Client)(nil)
	_ providers.ModelCatalogProvider = (*Client)(nil)
	_ providers.ErrorClassifier      = classifier{}
	_ providers.AccountProber        = (*Client)(nil)
	_ providers.CredentialImporter   = importer{}
	_ providers.ImportExporter       = importer{}
)
