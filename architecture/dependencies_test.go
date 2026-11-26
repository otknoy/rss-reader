package architecture_test

import (
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"testing"
)

var allowed = map[string]map[string]bool{
	"feeds":          {},
	"feeds/postgres": {"feeds": true},
	"articles":       {},
	"config":         {"ingestion": true},
	"ingestion":      {"feeds": true, "articles": true},
	"reading":        {"articles": true},
	"search":         {"articles": true},
	"workspace":      {"feeds": true, "articles": true},
}

func permitted(from, to string) bool {
	deps, known := allowed[from]
	_, targetKnown := allowed[to]
	return known && targetKnown && (from == to || deps[to])
}

func moduleName(path string) string {
	module, rest, _ := strings.Cut(path, "/")
	if adapter, _, _ := strings.Cut(rest, "/"); adapter == "postgres" {
		return module + "/postgres"
	}
	return module
}

func TestModuleDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-json", "./internal/...")
	cmd.Dir = ".."
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	const prefix = "rss-reader/internal/"
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	for {
		var pkg struct {
			ImportPath                         string
			Imports, TestImports, XTestImports []string
		}
		if err := decoder.Decode(&pkg); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		from := moduleName(strings.TrimPrefix(pkg.ImportPath, prefix))
		if _, known := allowed[from]; !known {
			t.Errorf("unknown module: %s", pkg.ImportPath)
		}
		imports := append(append(pkg.Imports, pkg.TestImports...), pkg.XTestImports...)
		for _, path := range imports {
			if !strings.HasPrefix(path, "rss-reader/") {
				continue
			}
			if !strings.HasPrefix(path, prefix) {
				t.Errorf("%s imports package outside modules: %s", pkg.ImportPath, path)
				continue
			}
			to := moduleName(strings.TrimPrefix(path, prefix))
			if !permitted(from, to) {
				t.Errorf("forbidden dependency: %s -> %s", pkg.ImportPath, path)
			}
		}
	}
}

func TestDependencyPolicy(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"articles", "articles", true},
		{"ingestion", "articles", true},
		{"search", "articles", true},
		{"articles", "feeds", false},
		{"feeds", "ingestion", false},
		{"feeds", "feeds/postgres", false},
		{"feeds/postgres", "feeds", true},
		{"ingestion", "feeds/postgres", false},
		{"search", "feeds", false},
		{"articles", "shared", false},
		{"unknown", "unknown", false},
	} {
		if got := permitted(tc.from, tc.to); got != tc.want {
			t.Errorf("%s -> %s: got %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
