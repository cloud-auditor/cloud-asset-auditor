package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/config"
)

// TestInit_IgnoresExtensionlessAuditorFile is the regression test for the CI
// smoke failure: in workspaces that contain a freshly-built `./auditor`
// binary, viper's bare-filename fallback (active when SetConfigType was
// called) would try to parse the ELF bytes as YAML and die with
// "yaml: control characters are not allowed". Init must look only at
// files with known config extensions.
func TestInit_IgnoresExtensionlessAuditorFile(t *testing.T) {
	dir := t.TempDir()
	// Mimic the CI workspace: a non-YAML file literally named "auditor"
	// in the current directory.
	if err := os.WriteFile(filepath.Join(dir, "auditor"), []byte("\x7fELF\x00not yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir()) // hide any real ~/.config/auditor.*

	if _, err := config.Init(""); err != nil {
		t.Errorf("Init must ignore extensionless files; got %v", err)
	}
}

// TestInit_LoadsYamlFromCwd proves the auto-detect path still works after
// the SetConfigType removal — auditor.yaml in cwd is found and parsed.
func TestInit_LoadsYamlFromCwd(t *testing.T) {
	dir := t.TempDir()
	body := "output:\n  format: csv\n"
	if err := os.WriteFile(filepath.Join(dir, "auditor.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())

	v, err := config.Init("")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetString("output.format"); got != "csv" {
		t.Errorf("output.format = %q, want csv", got)
	}
}

// TestInit_ExplicitConfigFile honors --config and detects the type from
// the file extension.
func TestInit_ExplicitConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.yaml")
	if err := os.WriteFile(path, []byte("audit:\n  timeout: 30m\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	v, err := config.Init(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetString("audit.timeout"); got != "30m" {
		t.Errorf("audit.timeout = %q, want 30m", got)
	}
}

// TestInit_MissingConfigFile_NotAnError mirrors the documented behavior:
// no config file in any search path is fine — flags + env can still drive
// the run.
func TestInit_MissingConfigFile_NotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())

	if _, err := config.Init(""); err != nil {
		t.Errorf("Init must tolerate missing config file; got %v", err)
	}
}

// TestInit_EnvVarOverridesConfigFile verifies the documented precedence
// chain at the env-vs-config-file boundary.
func TestInit_EnvVarOverridesConfigFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auditor.yaml"),
		[]byte("output:\n  format: csv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AUDITOR_OUTPUT_FORMAT", "json")

	v, err := config.Init("")
	if err != nil {
		t.Fatal(err)
	}
	if got := v.GetString("output.format"); got != "json" {
		t.Errorf("env should override config file; output.format = %q, want json", got)
	}
}
