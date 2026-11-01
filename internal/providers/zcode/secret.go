package zcode

// ZCode keeps its own sign-in — the Start Plan session and the coding plan's
// key — in ~/.zcode/v2/credentials.json, every value sealed as
// enc:v1:<iv>.<tag>.<data> (AES-256-GCM, base64url parts) under a key made from
// the machine it runs on. ZCode moved things around twice:
//
//   - through 3.14 the coding plan key sits in credentials.json as
//     account-provider:coding-plan:account:<site>-individual-coding-plan:...:api-key;
//   - 3.15 keeps no key there and uses the one in config.json, the provider's
//     options.apiKey (yetone/magpie#2130);
//   - 3.16 keeps neither: only a project-key-location:v1: entry naming the
//     organization, project and api-key id, and it fetches that key's secret
//     from the business API each time (magpie#410).
//
// The session (zcodejwttoken) is in credentials.json in all three, which is
// what this gateway runs on. Importing one of those files on a server needs the
// seed the client used: the operator supplies it as ZCODE_CREDENTIAL_SECRET
// (the secret itself) or ZCODE_CREDENTIAL_SEED (the whole fallback seed string,
// "zcode-credential-fallback:<platform>:<home>:<user>", which is what a client
// with no secret set uses). Only then can a sealed file be read at all.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// sealedValuePrefix marks a value ZCode sealed with the machine's key. The
// prefix is shared with credential.go's encV1Prefix; this name says what the
// value is rather than how it is stored.
const sealedValuePrefix = encV1Prefix

// projectKeyLocationPrefix names the 3.16 credential-store entry that holds no
// key, only where to find it.
const projectKeyLocationPrefix = "project-key-location:v1:"

// sealedValueSeeds returns the candidate seeds an operator configured, most
// specific first. Both are used as sha256(seed), which is the AES key ZCode
// derives.
func sealedValueSeeds() []string {
	var seeds []string
	for _, name := range []string{"ZCODE_CREDENTIAL_SECRET", "ZCODE_CREDENTIAL_SEED"} {
		if v := strings.TrimSpace(envOr(name, "")); v != "" {
			seeds = append(seeds, v)
		}
	}
	return seeds
}

// openSealedValue decrypts one enc:v1: value with a seed, returning the
// plaintext. A value that is not sealed, is malformed, or does not
// authenticate under this seed (GCM rejects a wrong key) reports false rather
// than an error: the caller tries the next seed and otherwise treats the
// document as unreadable.
func openSealedValue(value, seed string) (string, bool) {
	if !strings.HasPrefix(value, sealedValuePrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(value, sealedValuePrefix), ".")
	if len(parts) != 3 {
		return "", false
	}
	decoded := make([][]byte, 3)
	for i, part := range parts {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part, "="))
		if err != nil {
			return "", false
		}
		decoded[i] = raw
	}
	iv, tag, data := decoded[0], decoded[1], decoded[2]
	if len(iv) == 0 || len(tag) == 0 {
		return "", false
	}
	key := sha256.Sum256([]byte(seed))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	combined := make([]byte, 0, len(data)+len(tag))
	combined = append(combined, data...)
	combined = append(combined, tag...)
	plain, err := gcm.Open(nil, iv, combined, nil)
	if err != nil {
		return "", false
	}
	return string(plain), true
}

// unsealDocument returns a copy of a ZCode local document with every sealed
// value it could open replaced by its plaintext, plus how many values stayed
// sealed. The original is left untouched: an import must never half-modify the
// document it was handed.
func unsealDocument(doc any) (any, int) {
	seeds := sealedValueSeeds()
	if len(seeds) == 0 {
		return doc, countSealedValues(doc)
	}
	left := 0
	var walk func(value any) any
	walk = func(value any) any {
		switch typed := value.(type) {
		case string:
			if !strings.HasPrefix(typed, sealedValuePrefix) {
				return typed
			}
			for _, seed := range seeds {
				if plain, ok := openSealedValue(typed, seed); ok {
					return plain
				}
			}
			left++
			return typed
		case map[string]any:
			out := make(map[string]any, len(typed))
			for key, item := range typed {
				// The credential store's keys carry material too: a
				// project-key-location:v1: entry names the key's location in
				// its *name*, so keep the name and open the value.
				out[key] = walk(item)
			}
			return out
		case []any:
			out := make([]any, len(typed))
			for i, item := range typed {
				out[i] = walk(item)
			}
			return out
		default:
			return value
		}
	}
	return walk(doc), left
}

// countSealedValues counts the sealed values in a document.
func countSealedValues(doc any) int {
	total := 0
	walkJSONObjects(doc, func(m map[string]any) bool {
		for _, value := range m {
			if s, ok := value.(string); ok && strings.HasPrefix(s, sealedValuePrefix) {
				total++
			}
		}
		return true
	})
	return total
}

// hasProjectKeyLocation reports whether the document carries ZCode 3.16's
// key-location entries: the file names where the coding plan's key is instead
// of holding it.
func hasProjectKeyLocation(doc any) bool {
	found := false
	walkJSONObjects(doc, func(m map[string]any) bool {
		for key := range m {
			if strings.HasPrefix(key, projectKeyLocationPrefix) {
				found = true
				return false
			}
		}
		return !found
	})
	return found
}
