// Command sync-shared bundles the first-party packages needed by each application.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func main() {
	check := flag.Bool("check", false, "report stale application dependency snapshots")
	flag.Parse()
	if err := sync(*check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func sync(check bool) error {
	apps := []struct {
		name     string
		packages []string
	}{
		{"agent-admin", []string{"app", "codex", "keys", "storage"}},
		{"agent-gateway", []string{"app", "codex", "keys", "relay", "storage"}},
		{"agent-forwarder", []string{"app", "codex", "keys", "relay", "storage"}},
		{"agent-billing", []string{"app"}},
		{"agent-portal", []string{"app"}},
		{"agent-admin-web", []string{"app"}},
	}
	for _, app := range apps {
		target := filepath.Join("apps", app.name, "deps", "shared")
		files := map[string]bool{}
		for _, name := range []string{"go.mod", "go.sum"} {
			if err := copyFile(name, target, files, check); err != nil {
				return err
			}
		}
		for _, name := range app.packages {
			source := filepath.Join("shared", name)
			err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				relative, err := filepath.Rel("shared", path)
				if err != nil {
					return err
				}
				return copyFile(relative, target, files, check)
			})
			if err != nil {
				return err
			}
		}
		if err := filepath.WalkDir(target, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && !files[path] {
				return fmt.Errorf("remove obsolete snapshot file %s", path)
			}
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(name, target string, files map[string]bool, check bool) error {
	source := filepath.Join("shared", name)
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	dest := filepath.Join(target, name)
	files[dest] = true
	current, err := os.ReadFile(dest)
	if err == nil && bytes.Equal(bytes.ReplaceAll(current, []byte("\r\n"), []byte("\n")), bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))) {
		return nil
	}
	if check {
		return fmt.Errorf("%s is out of date, run go run ./tools/sync-shared/main.go", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0644)
}
