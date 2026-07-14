package config

import (
	"os"
	"testing"
)

func TestInitAndLoad(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	if err := Init(root, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Endpoint != cfg.Endpoint || loaded.OutputDir != cfg.OutputDir {
		t.Fatalf("loaded config = %+v", loaded)
	}
	if _, err := os.Stat(OutputPath(root, cfg)); err != nil {
		t.Fatalf("output directory was not created: %v", err)
	}
}

func TestValidateRejectsUnsafeOutputDirectory(t *testing.T) {
	cfg := Default()
	cfg.OutputDir = "../outside"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unsafe output directory to be rejected")
	}
}

func TestValidateRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"127.0.0.1:6806", "http://", "http://127.0.0.1:6806?token=secret"} {
		cfg := Default()
		cfg.Endpoint = endpoint
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected endpoint %q to be rejected", endpoint)
		}
	}
}
