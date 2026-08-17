package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestNinaDateFolderBase(t *testing.T) {
	cases := []struct {
		path     string
		wantBase string
		wantOK   bool
	}{
		{filepath.Join("/data", "2026-08-16", "LIGHT"), "/data", true},
		{filepath.Join("/data", "sub", "2026-08-16", "LIGHT"), filepath.Join("/data", "sub"), true},
		{filepath.Join("/data", "2026-08-16", "LIGHT") + "/", "/data", true}, // trailing slash
		{filepath.Join("/data", "2026-08-16", "DARK"), "", false},            // wrong leaf
		{filepath.Join("/data", "foo", "LIGHT"), "", false},                  // parent not a date
		{filepath.Join("/data", "2026-13-40", "LIGHT"), "", false},           // invalid date
		{filepath.Join("/home", "user", "camera", "input"), "", false},       // non-NINA
		{filepath.Join("/data", "LIGHT"), "", false},                         // no date parent
	}
	for _, c := range cases {
		base, ok := ninaDateFolderBase(c.path)
		if ok != c.wantOK || (ok && base != c.wantBase) {
			t.Errorf("ninaDateFolderBase(%q) = (%q, %v), want (%q, %v)",
				c.path, base, ok, c.wantBase, c.wantOK)
		}
	}
}

func TestCandidateScanDirs(t *testing.T) {
	// Non-NINA path: exactly the configured directory, nothing guessed.
	ac := &AstroCam{config: &Config{CameraDirectory: filepath.Join("/home", "user", "input")}}
	dirs := ac.candidateScanDirs()
	if len(dirs) != 1 || dirs[0] != ac.config.CameraDirectory {
		t.Errorf("non-NINA path: got %v, want just the configured dir", dirs)
	}

	// NINA path whose date matches the current NINA night: no duplicate guess.
	today := time.Now().Add(-ninaDateFolderOffset).Format("2006-01-02")
	configuredToday := filepath.Join("/data", today, "LIGHT")
	ac = &AstroCam{config: &Config{CameraDirectory: configuredToday}}
	dirs = ac.candidateScanDirs()
	if len(dirs) != 1 || dirs[0] != configuredToday {
		t.Errorf("NINA path (current date): got %v, want just the configured dir", dirs)
	}

	// NINA path with a stale (different) date: both configured and guessed.
	configuredStale := filepath.Join("/data", "2020-01-01", "LIGHT")
	ac = &AstroCam{config: &Config{CameraDirectory: configuredStale}}
	dirs = ac.candidateScanDirs()
	guessed := filepath.Join("/data", today, "LIGHT")
	if len(dirs) != 2 || dirs[0] != configuredStale || dirs[1] != guessed {
		t.Errorf("NINA path (stale date): got %v, want [%q %q]", dirs, configuredStale, guessed)
	}
}

func TestUploadServerCandidates(t *testing.T) {
	// No fallback -> single candidate.
	ac := &AstroCam{config: &Config{Server: "https://primary/upload.py"}}
	if got := ac.uploadServerCandidates(); len(got) != 1 || got[0] != "https://primary/upload.py" {
		t.Errorf("no fallback: got %v", got)
	}
	// Distinct fallback -> two candidates in order.
	ac = &AstroCam{config: &Config{Server: "https://primary/upload.py", FallbackServer: "https://backup/upload.py"}}
	got := ac.uploadServerCandidates()
	if len(got) != 2 || got[0] != "https://primary/upload.py" || got[1] != "https://backup/upload.py" {
		t.Errorf("with fallback: got %v", got)
	}
	// Fallback equal to primary -> deduped to one.
	ac = &AstroCam{config: &Config{Server: "https://primary/upload.py", FallbackServer: "https://primary/upload.py"}}
	if got := ac.uploadServerCandidates(); len(got) != 1 {
		t.Errorf("duplicate fallback: got %v", got)
	}
}
