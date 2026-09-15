// Package lang holds the registry of supported languages and how each one
// is compiled and run inside the sandbox.
package lang

import "sort"

// Spec describes one supported language.
//
// Compile and Run are /bin/sh fragments executed inside the sandbox with the
// working directory set to the tmpfs workdir. An empty Compile means the
// language is interpreted and the run step is timed on its own.
type Spec struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	Image    string `json:"image"`
	Filename string `json:"filename"`
	Compile  string `json:"-"`
	Run      string `json:"-"`
	// Notes surfaces per-language rules the caller must follow, e.g. Java's
	// mandatory class name.
	Notes string `json:"notes,omitempty"`
}

// Compiled reports whether the language needs a separate compile step.
func (s Spec) Compiled() bool { return s.Compile != "" }

var registry = map[string]Spec{
	"python": {
		ID:       "python",
		Name:     "Python",
		Version:  "3.12",
		Image:    "python:3.12-alpine",
		Filename: "main.py",
		// -I isolates the interpreter: no user site-packages, no cwd on
		// sys.path, no PYTHON* env influence from the submission.
		Run: "python3 -I main.py",
	},
	"node": {
		ID:       "node",
		Name:     "Node.js",
		Version:  "22",
		Image:    "node:22-alpine",
		Filename: "main.js",
		Run:      "node main.js",
	},
	"c": {
		ID:       "c",
		Name:     "C",
		Version:  "GCC 14 (C17)",
		Image:    "gcc:14",
		Filename: "main.c",
		Compile:  "gcc -std=c17 -O2 -static -o prog main.c -lm",
		Run:      "./prog",
	},
	"cpp": {
		ID:       "cpp",
		Name:     "C++",
		Version:  "GCC 14 (C++20)",
		Image:    "gcc:14",
		Filename: "main.cpp",
		Compile:  "g++ -std=c++20 -O2 -static -o prog main.cpp",
		Run:      "./prog",
	},
	"java": {
		ID:       "java",
		Name:     "Java",
		Version:  "Temurin 21",
		Image:    "eclipse-temurin:21-jdk-alpine",
		Filename: "Main.java",
		Compile:  "javac Main.java",
		// -XX:-UsePerfData stops the JVM writing to /tmp/hsperfdata_*, which
		// costs a syscall storm on tmpfs and leaks nothing useful here.
		Run:   "java -XX:-UsePerfData -Xshare:auto Main",
		Notes: "The public class must be named Main.",
	},
}

// Get returns the spec for id and whether it is known.
func Get(id string) (Spec, bool) {
	s, ok := registry[id]
	return s, ok
}

// All returns every supported language, ordered by id for stable output.
func All() []Spec {
	out := make([]Spec, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Images returns the distinct set of images the worker needs available.
func Images() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range All() {
		if _, ok := seen[s.Image]; ok {
			continue
		}
		seen[s.Image] = struct{}{}
		out = append(out, s.Image)
	}
	return out
}
