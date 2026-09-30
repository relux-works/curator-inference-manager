package main

import "testing"

func TestFakeRuntimePublishesFirstCommaDelimitedAlias(t *testing.T) {
	if got := reportedModelID("", "model-from-argv", "first-alias,second-alias"); got != "first-alias" {
		t.Fatalf("llama-server alias list published %q, want first alias", got)
	}
}

func TestFakeRuntimeUsesModelBasenameWithoutAnAlias(t *testing.T) {
	if got := reportedModelID("", "/models/Qwen-Model", ""); got != "Qwen-Model" {
		t.Fatalf("model server reported %q, want model basename", got)
	}
}
