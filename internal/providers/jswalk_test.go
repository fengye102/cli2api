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
