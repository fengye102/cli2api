package loomy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// fakeStore is the in-memory Store every test uses.
type fakeStore struct {
	mu       sync.Mutex
	accounts map[string]accounts.Account
	payloads map[string][]byte
	formats  map[string]string
	observed []string
	secrets  map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts: map[string]accounts.Account{},
		payloads: map[string][]byte{},
		formats:  map[string]string{},
		secrets:  map[string]string{},
	}
}

// addAccount creates the account row a login needs before any credential
// exists.
func (s *fakeStore) addAccount(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[id] = accounts.Account{ID: id, Provider: "loomy", ProviderRegion: "cn"}
}

func (s *fakeStore) add(id string, cred Credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[id] = accounts.Account{ID: id, Provider: "loomy", ProviderRegion: "cn"}
	payload, _ := EncodeCredential(cred)
	s.payloads[id] = payload
	s.formats[id] = CredentialFormat
}

func (s *fakeStore) Get(_ context.Context, id string) (accounts.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	account, ok := s.accounts[id]
	if !ok {
		return accounts.Account{}, fmt.Errorf("account %s: record not found", id)
	}
	return account, nil
}

func (s *fakeStore) LoadCredentialPayload(_ context.Context, accountID string) (string, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, ok := s.payloads[accountID]
	if !ok {
		return "", nil, fmt.Errorf("credential for %s: record not found", accountID)
	}
	return s.formats[accountID], payload, nil
}

func (s *fakeStore) SaveCredentialPayload(_ context.Context, accountID, format string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads[accountID] = append([]byte(nil), payload...)
	s.formats[accountID] = format
	return nil
}

func (s *fakeStore) Observe(_ context.Context, id, remoteUID, status, lastError, lastKind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = append(s.observed, fmt.Sprintf("%s|%s|%s|%s|%s", id, remoteUID, status, lastError, lastKind))
	return nil
}

func (s *fakeStore) GetSecret(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.secrets[key]
	return value, ok, nil
}

func (s *fakeStore) credential(t interface{ Fatalf(string, ...any) }, accountID string) Credential {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload, ok := s.payloads[accountID]
	if !ok {
		t.Fatalf("no stored credential for %s", accountID)
	}
	cred, err := DecodeCredential(payload)
	if err != nil {
		t.Fatalf("decode stored credential: %v", err)
	}
	return cred
}

// farFuture is an expiry that never trips the proactive renew window.
func farFuture() int64 { return time.Now().Add(365 * 24 * time.Hour).Unix() }

func decodeB64(value string) ([]byte, error) { return base64.StdEncoding.DecodeString(value) }

func mustJSON(t interface{ Fatalf(string, ...any) }, value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// testRSAKey is the account service's public key stand-in: the adapter only
// needs *some* RSA key it can encrypt to, and the test needs the private half
// to prove the ciphertext decrypts back to the password.
type testRSAKey struct {
	key       *rsa.PrivateKey
	publicB64 string
}

func newTestRSAKey(t interface{ Fatalf(string, ...any) }) testRSAKey {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return testRSAKey{key: key, publicB64: base64.StdEncoding.EncodeToString(der)}
}

func (k testRSAKey) size() int { return k.key.Size() }

func (k testRSAKey) decrypt(t interface{ Fatalf(string, ...any) }, ciphertext []byte) string {
	plain, err := rsa.DecryptPKCS1v15(rand.Reader, k.key, ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return string(plain)
}
