package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPackageIncludesOnlySelectedRuntimeAndDeclaredFiles(t *testing.T) {
	root := t.TempDir()
	dist, ui := filepath.Join(root, "dist"), filepath.Join(root, "ui")
	for path, content := range map[string]string{
		filepath.Join(dist, "runtimes/windows-amd64/oai-basispoints.exe"): "selected runtime",
		filepath.Join(dist, "runtimes/linux-amd64/oai-basispoints"):       "stale runtime",
		filepath.Join(ui, "index.html"):                                   "configuration",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{SchemaVersion: 1, ID: "local.test", Version: "1.0.0",
		Capabilities: []map[string]string{{"id": "openai.oauth.outbound_transport.v1"}},
		UI:           UIManifest{Entrypoint: "ui/index.html"},
		Runtimes:     map[string]RuntimeEntry{"linux-amd64": {Path: "runtimes/linux-amd64/oai-basispoints"}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(source, raw, 0644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "custom.s2plugin")
	if err := run(source, dist, ui, "windows/amd64", output, "", "", "", true); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name == "manifest.json" {
			raw, err := readZipEntry(entry)
			if err != nil {
				t.Fatal(err)
			}
			m = Manifest{}
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			if len(m.Runtimes) != 1 {
				t.Errorf("stale runtime declarations: %v", m.Runtimes)
			}
		} else if entry.Name != "ui/index.html" && entry.Name != "runtimes/windows-amd64/oai-basispoints.exe" {
			t.Errorf("undeclared or unselected file in package: %s", entry.Name)
		}
	}
}

func TestReadManifestRejectsTrailingJSON(t *testing.T) {
	source := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(source, []byte("{} {}"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(source); err == nil {
		t.Fatal("accepted a second JSON value")
	}
}
