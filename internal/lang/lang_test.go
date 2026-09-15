package lang

import (
	"strings"
	"testing"
)

func TestRegistryCoversAdvertisedLanguages(t *testing.T) {
	want := []string{"c", "cpp", "java", "node", "python"}
	got := All()
	if len(got) != len(want) {
		t.Fatalf("registry has %d languages, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("All()[%d].ID = %q, want %q (All must be sorted)", i, got[i].ID, id)
		}
	}
}

func TestEverySpecIsRunnable(t *testing.T) {
	for _, s := range All() {
		if s.Image == "" || s.Filename == "" || s.Run == "" {
			t.Errorf("%s: incomplete spec %+v", s.ID, s)
		}
		// The sandbox script substitutes these into `sh -c`, so a stray
		// double quote would break out of the command string.
		if strings.Contains(s.Run, `"`) || strings.Contains(s.Compile, `"`) {
			t.Errorf("%s: command contains a double quote, which breaks sh -c quoting", s.ID)
		}
	}
}

func TestCompiledLanguagesAreMarked(t *testing.T) {
	compiled := map[string]bool{"c": true, "cpp": true, "java": true}
	for _, s := range All() {
		if s.Compiled() != compiled[s.ID] {
			t.Errorf("%s: Compiled() = %v, want %v", s.ID, s.Compiled(), compiled[s.ID])
		}
	}
}

// Java's compiler derives the class file name from the source, so the run
// command only works if the file and the class agree.
func TestJavaFilenameMatchesRunCommand(t *testing.T) {
	s, ok := Get("java")
	if !ok {
		t.Fatal("java missing from registry")
	}
	if s.Filename != "Main.java" || !strings.HasSuffix(s.Run, "Main") {
		t.Errorf("java spec is inconsistent: file=%q run=%q", s.Filename, s.Run)
	}
	if s.Notes == "" {
		t.Error("the Main class requirement must be surfaced to API callers")
	}
}

func TestImagesAreDeduplicated(t *testing.T) {
	// C and C++ share the gcc image; pulling it twice at warm-up is waste.
	imgs := Images()
	seen := map[string]bool{}
	for _, i := range imgs {
		if seen[i] {
			t.Errorf("image %q listed twice", i)
		}
		seen[i] = true
	}
	if len(imgs) != 4 {
		t.Errorf("got %d distinct images, want 4 (c and cpp share one)", len(imgs))
	}
}

func TestGetUnknownLanguage(t *testing.T) {
	if _, ok := Get("brainfuck"); ok {
		t.Error("Get returned ok for an unregistered language")
	}
}
