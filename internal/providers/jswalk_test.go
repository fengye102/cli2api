package providers

import (
	"testing"
)

func TestUnwrapJSONValueNestedString(t *testing.T) {
	raw := []byte(`{"iCubeAuthInfo://icube.cloudide": "{\"accessToken\":\"at\",\"refreshToken\":\"rt\"}"}`)
	value, err := UnwrapJSONValue(raw)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	auth, ok := value.(map[string]any)["iCubeAuthInfo://icube.cloudide"].(map[string]any)
	if !ok {
		t.Fatalf("expected unwrapped inner object, got %#v", value)
	}
	if auth["accessToken"] != "at" {
		t.Fatalf("unexpected inner: %#v", auth)
	}
}

func TestDeepFindAuthObjectOutermostFirst(t *testing.T) {
	raw := []byte(`{"data":{"auth":{"accessToken":"inner"}},"access_token":"outer"}`)
	value, err := UnwrapJSONValue(raw)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	found := DeepFindAuthObject(value)
	if found == nil {
		t.Fatalf("expected auth object")
	}
	if got := DeepPickString(found, "access_token"); got != "outer" {
		t.Fatalf("want outermost token, got %q", got)
	}
}

func TestDeepPickStringCaseInsensitive(t *testing.T) {
	value, err := UnwrapJSONValue([]byte(`{"account":{"userInfo":{"Email":"a@b.com"}}}`))
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if got := DeepPickString(value, "email"); got != "a@b.com" {
		t.Fatalf("want a@b.com, got %q", got)
	}
}

// Go randomizes map iteration, so "first match wins" used to mean "a random
// account wins" whenever one paste carried several candidates. Sibling
// objects are now walked in sorted key order.
func TestDeepPickStringDeterministicAcrossSiblings(t *testing.T) {
	value, err := UnwrapJSONValue([]byte(`{"z":{"accessToken":"z-token"},"m":{"accessToken":"m-token"},"a":{"accessToken":"a-token"}}`))
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	const want = "a-token"
	for i := 0; i < 500; i++ {
		if got := DeepPickString(value, "accessToken"); got != want {
			t.Fatalf("run %d: got %q want %q", i, got, want)
		}
	}
}

func TestDeepFindAuthObjectDeterministicAcrossSiblings(t *testing.T) {
	value, err := UnwrapJSONValue([]byte(`{"z":{"token":"z"},"m":{"token":"m"},"a":{"token":"a"}}`))
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	for i := 0; i < 500; i++ {
		found := DeepFindAuthObject(value)
		if found == nil || found["token"] != "a" {
			t.Fatalf("run %d: picked %#v", i, found)
		}
	}
}

// The caller's field-name order expresses preference; it must win over both
// map order and the case-insensitive fallback.
func TestDeepPickPrefersCallerNameOrder(t *testing.T) {
	value, err := UnwrapJSONValue([]byte(`{"access_token":"snake","accessToken":"camel","expires_at":111,"expiresAt":222}`))
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	for i := 0; i < 200; i++ {
		if got := DeepPickString(value, "accessToken", "access_token"); got != "camel" {
			t.Fatalf("run %d: want camel, got %q", i, got)
		}
		if got := DeepPickString(value, "access_token", "accessToken"); got != "snake" {
			t.Fatalf("run %d: want snake, got %q", i, got)
		}
		if got := DeepPickInt(value, "expiresAt", "expires_at"); got != 222 {
			t.Fatalf("run %d: want 222, got %d", i, got)
		}
		if got := DeepPickInt(value, "expires_at", "expiresAt"); got != 111 {
			t.Fatalf("run %d: want 111, got %d", i, got)
		}
	}
}

func TestDeepPickIntNumericString(t *testing.T) {
	value, err := UnwrapJSONValue([]byte(`{"data":{"ExpiresAt":"1780000000000"}}`))
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if got := DeepPickInt(value, "expiresAt"); got != 1780000000000 {
		t.Fatalf("want 1780000000000, got %d", got)
	}
	if got := DeepPickInt(value, "missing"); got != 0 {
		t.Fatalf("want 0 for absent field, got %d", got)
	}
}
