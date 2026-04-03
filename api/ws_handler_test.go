package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupPluginValidatorScript(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Setenv("PLUGIN_VALIDATOR_SCRIPT", filepath.Join(wd, "..", "config", "plugin-validator.sh"))
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestValidateForgedPluginDirSuccess(t *testing.T) {
	setupPluginValidatorScript(t)
	pluginDir := t.TempDir()

	writeTestFile(t, filepath.Join(pluginDir, "TimerPlugin.tsx"), "export function TimerPlugin() { return null; }\n")
	writeTestFile(t, filepath.Join(pluginDir, "TimerView.tsx"), "export function TimerView() { return null; }\n")
	writeTestFile(t, filepath.Join(pluginDir, "styles", "timer.css"), ".timer { color: var(--on); }\n")
	writeTestFile(t, filepath.Join(pluginDir, "bundle.js"), "(() => { console.log('ok'); })();\n")

	result, err := validateForgedPluginDir(pluginDir)
	if err != nil {
		t.Fatalf("validateForgedPluginDir returned error: %v", err)
	}
	if result.entryFile != "TimerPlugin.tsx" {
		t.Fatalf("unexpected entry file: %s", result.entryFile)
	}
}

func TestValidateForgedPluginDirRejectsNestedForbiddenImport(t *testing.T) {
	setupPluginValidatorScript(t)
	pluginDir := t.TempDir()

	writeTestFile(t, filepath.Join(pluginDir, "TimerPlugin.tsx"), "export function TimerPlugin() { return null; }\n")
	writeTestFile(t, filepath.Join(pluginDir, "TimerView.tsx"), "export function TimerView() { return null; }\n")
	writeTestFile(t, filepath.Join(pluginDir, "nested", "Bad.tsx"), "import { i18n } from '@cubelv/sdk';\nexport const Bad = () => null;\n")
	writeTestFile(t, filepath.Join(pluginDir, "bundle.js"), "(() => { console.log('ok'); })();\n")

	_, err := validateForgedPluginDir(pluginDir)
	if err == nil {
		t.Fatalf("expected validation to fail")
	}
	if !strings.Contains(err.Error(), "i18n import") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateForgedPluginDirRejectsMainTsx(t *testing.T) {
	setupPluginValidatorScript(t)
	pluginDir := t.TempDir()

	writeTestFile(t, filepath.Join(pluginDir, "TimerPlugin.tsx"), "export function TimerPlugin() { return null; }\n")
	writeTestFile(t, filepath.Join(pluginDir, "TimerView.tsx"), "export function TimerView() { return null; }\n")
	writeTestFile(t, filepath.Join(pluginDir, "main.tsx"), "export {};\n")
	writeTestFile(t, filepath.Join(pluginDir, "bundle.js"), "(() => { console.log('ok'); })();\n")

	_, err := validateForgedPluginDir(pluginDir)
	if err == nil {
		t.Fatalf("expected validation to fail")
	}
	if !strings.Contains(err.Error(), "main.tsx") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateForgedPluginDirDoesNotFallbackToSiblingDirectory(t *testing.T) {
	setupPluginValidatorScript(t)
	workDir := t.TempDir()
	targetDir := filepath.Join(workDir, "plugins", "expected-plugin")
	siblingDir := filepath.Join(workDir, "plugins", "other-plugin")

	writeTestFile(t, filepath.Join(targetDir, "TimerView.tsx"), "export function TimerView() { return null; }\n")
	writeTestFile(t, filepath.Join(targetDir, "timer.css"), ".timer { color: var(--on); }\n")
	writeTestFile(t, filepath.Join(targetDir, "bundle.js"), "(() => { console.log('target'); })();\n")

	writeTestFile(t, filepath.Join(siblingDir, "OtherPlugin.tsx"), "export function OtherPlugin() { return null; }\n")
	writeTestFile(t, filepath.Join(siblingDir, "OtherView.tsx"), "export function OtherView() { return null; }\n")
	writeTestFile(t, filepath.Join(siblingDir, "other.css"), ".other { color: var(--on); }\n")
	writeTestFile(t, filepath.Join(siblingDir, "bundle.js"), "(() => { console.log('sibling'); })();\n")

	_, err := validateForgedPluginDir(targetDir)
	if err == nil {
		t.Fatalf("expected target directory validation to fail")
	}
	if !strings.Contains(err.Error(), "入口檔") {
		t.Fatalf("unexpected error: %v", err)
	}
}
