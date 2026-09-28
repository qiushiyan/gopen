package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// $BROWSER outranks the platform opener: over SSH the office mini sets it to
// browser-clip so a URL reaches the laptop, while `open` would draw on the
// mini's own screen. $GOPEN_BROWSER outranks both.
func TestBrowserPrecedence(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "opened")
	for _, name := range []string{"open", "xdg-open", "browser"} {
		script := "#!/bin/sh\necho " + name + " >> '" + log + "'\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	platform := "xdg-open"
	if runtime.GOOS == "darwin" {
		platform = "open"
	}
	for _, c := range []struct{ gopen, browser, want string }{
		{"", filepath.Join(dir, "browser"), "browser"},
		{"", "", platform},
		{filepath.Join(dir, "xdg-open"), filepath.Join(dir, "browser"), "xdg-open"},
	} {
		os.Remove(log)
		t.Setenv("GOPEN_BROWSER", c.gopen)
		t.Setenv("BROWSER", c.browser)
		var stderr bytes.Buffer
		if err := openBrowser("https://example.invalid", &stderr); err != nil {
			t.Fatalf("%+v: %v %s", c, err, &stderr)
		}
		got, _ := os.ReadFile(log)
		if strings.TrimSpace(string(got)) != c.want {
			t.Errorf("GOPEN_BROWSER=%q BROWSER=%q opened %q, want %q", c.gopen, c.browser, got, c.want)
		}
	}
}
