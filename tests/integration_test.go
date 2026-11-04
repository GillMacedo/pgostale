// Package tests runs pgostale the way the skill does: as a built binary,
// on the example ledger service and its real CPU profiles, and on broken
// inputs that must never be reported as matching.
package tests

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pgostale-bin")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "pgostale")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Build the single file, as the skill runs it.
	out, err := exec.Command("go", "build", "-o", bin, "../plugin/scripts/pgostale.go").CombinedOutput()
	if err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func abs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var (
	profileV1   = "../examples/profiles/ledger-v1.pprof"
	profileTest = "../examples/profiles/ledger-v1-test.pprof"
)

// pgostale runs the binary and returns stdout, stderr and the exit code.
func pgostale(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

type report struct {
	Score      float64
	ScoreBasis string
	HotEdges   int                           `json:"moduleHotEdges"`
	HotMatched int                           `json:"moduleHotEdgesMatched"`
	Below      bool                          `json:"belowMin"`
	Missing    []struct{ Name, Hint string } `json:"missingFunctions"`
	Lost       []struct {
		Caller, Callee, Reason, Where, Now string
		Offset                             int
	} `json:"lostEdges"`
}

func check(t *testing.T, dir, profile string, extra ...string) (report, string, int) {
	t.Helper()
	args := append([]string{"check", "-C", dir, "--profile", abs(t, profile), "--json"}, extra...)
	out, errOut, code := pgostale(t, args...)
	var r report
	if code != 2 {
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("bad JSON (%v): %s\n%s", err, out, errOut)
		}
	}
	return r, errOut, code
}

func TestHealthyProfileStaysQuiet(t *testing.T) {
	for _, dir := range []string{"../examples/ledger/v1", "../examples/ledger/comments"} {
		r, errOut, code := check(t, dir, profileV1, "--fail")
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", dir, code, errOut)
		}
		if r.Score != 100 || len(r.Lost) != 0 || len(r.Missing) != 0 || r.HotEdges < 10 {
			t.Errorf("%s: %+v", dir, r)
		}
		out, _, code := pgostale(t, "check", "-C", dir, "--profile", abs(t, profileV1))
		if code != 0 || !strings.Contains(out, "Every call edge in this module still matches the code.") {
			t.Errorf("%s: exit %d\n%s", dir, code, out)
		}
	}
}

func TestStaleProfileIsReported(t *testing.T) {
	dir := "../examples/ledger/v2"
	r, errOut, code := check(t, dir, profileV1)
	if code != 0 {
		t.Fatalf("without --fail: exit %d: %s", code, errOut)
	}
	if r.Score > 60 || !r.Below {
		t.Errorf("score %v", r.Score)
	}
	hints := map[string]string{}
	for _, m := range r.Missing {
		hints[m.Name] = m.Hint
	}
	for name, want := range map[string]string{
		"main.Digest":                        "moved to another package: example.com/ledger/parse.Digest",
		"example.com/ledger/parse.ParseLine": "probably renamed to example.com/ledger/parse.Line",
		"main.Run.func1":                     "a closure",
	} {
		if !strings.Contains(hints[name], want) {
			t.Errorf("%s: hint %q, want %q", name, hints[name], want)
		}
	}
	changed := false
	for _, l := range r.Lost {
		if l.Reason == "callee-changed" && l.Caller == "example.com/ledger/book.(*Book).Apply" && l.Now == "Adjust" &&
			strings.HasSuffix(filepath.ToSlash(l.Where), "v2/book/book.go:62") {
			changed = true
		}
	}
	if !changed {
		t.Errorf("Apply +6 is not reported as now calling Adjust: %+v", r.Lost)
	}
	if _, _, code := check(t, dir, profileV1, "--fail"); code != 1 {
		t.Errorf("--fail: exit %d, want 1", code)
	}
	if _, _, code := check(t, dir, profileV1, "--fail", "--min", "40"); code != 0 {
		t.Errorf("--fail --min 40: exit %d, want 0", code)
	}
}

func TestTestBinaryProfile(t *testing.T) {
	r, _, code := check(t, "../examples/ledger/v1", profileTest, "--fail")
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	found := false
	for _, m := range r.Missing {
		if m.Name == "example.com/ledger.Run" && strings.Contains(m.Hint, "go test binary") {
			found = true
		}
	}
	if !found {
		t.Errorf("no go test hint: %+v", r.Missing)
	}
}

// copyTree copies dir into a new temporary directory.
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		os.MkdirAll(filepath.Dir(target), 0o755)
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestDefaultPGOIsFound(t *testing.T) {
	dir := copyTree(t, "../examples/ledger/v1")
	b, _ := os.ReadFile(profileV1)
	os.WriteFile(filepath.Join(dir, "default.pgo"), b, 0o644)
	out, errOut, code := pgostale(t, "check", "-C", dir, "--fail")
	if code != 0 || !strings.Contains(out, "default.pgo") || !strings.Contains(out, "100.0%") {
		t.Errorf("exit %d\n%s\n%s", code, out, errOut)
	}
	// Without a default.pgo, it must say so rather than check nothing.
	os.Remove(filepath.Join(dir, "default.pgo"))
	if _, errOut, code := pgostale(t, "check", "-C", dir); code != 2 || !strings.Contains(errOut, "default.pgo") {
		t.Errorf("no profile: exit %d: %s", code, errOut)
	}
}

// crashProfile runs a program that fails while profiling and returns what
// it left in the profile file. With exit "fatal" it calls log.Fatal, which
// skips deferred calls, so the profile is never written. With "panic" the
// deferred StopCPUProfile still runs, leaving a valid profile of the short
// time before the crash.
func crashProfile(t *testing.T, exit string) []byte {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/crash\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

import (
	"log"
	"os"
	"runtime/pprof"
)

func main() {
	f, _ := os.Create(os.Args[1])
	pprof.StartCPUProfile(f)
	defer pprof.StopCPUProfile()
	x := 0
	for i := 0; i < 50000000; i++ {
		x += i % 7
	}
	if os.Args[2] == "fatal" {
		log.Fatal("failed: ", x)
	}
	var s []int
	_ = s[x%2+5] // panics
}
`), 0o644)
	prof := filepath.Join(dir, "crash.pprof")
	cmd := exec.Command("go", "run", ".", prof, exit)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("the crash program didn't fail:\n%s", out)
	}
	b, err := os.ReadFile(prof)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestBadProfilesAreRefused gives a real package with each broken profile.
func TestBadProfilesAreRefused(t *testing.T) {
	real, err := os.ReadFile(profileV1)
	if err != nil {
		t.Fatal(err)
	}
	buildOut, _ := exec.Command("go", "build", "-o", os.DevNull, "./does-not-exist").CombinedOutput()
	cases := map[string][]byte{
		"empty":              {},
		"garbage":            []byte("\x00\x01garbage\xff\xfe" + strings.Repeat("zz", 100)),
		"cut in half":        real[:len(real)/2],
		"cut near the end":   real[:len(real)-1],
		"failed build":       buildOut,
		"log.Fatal run":      crashProfile(t, "fatal"),
		"panicked run":       crashProfile(t, "panic"),
		"preprofile text":    []byte("GO PREPROFILE V1\nmain.main\nmain.Run\n19 100\n"),
		"json":               []byte(`{"edges": []}`),
		"source code":        []byte("package main\n\nfunc main() {}\n"),
		"double gzip header": append([]byte{0x1f, 0x8b}, real...),
	}
	dir := t.TempDir()
	for name, data := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		os.WriteFile(p, data, 0o644)
		for _, extra := range [][]string{nil, {"--fail"}, {"--json"}} {
			args := append([]string{"check", "-C", "../examples/ledger/v1", "--profile", p}, extra...)
			out, errOut, code := pgostale(t, args...)
			if code != 2 || out != "" || !strings.Contains(errOut, "can't check") {
				t.Errorf("%s %v: exit %d, stdout %q, stderr %q", name, extra, code, out, errOut)
			}
		}
	}
}

// TestBadSourcesAreRefused gives the real profile with each broken source tree.
func TestBadSourcesAreRefused(t *testing.T) {
	v1 := "../examples/ledger/v1"
	mains, _ := os.ReadFile(filepath.Join(v1, "main.go"))
	cases := map[string]func(dir string){
		"syntax error": func(dir string) {
			os.WriteFile(filepath.Join(dir, "book", "book.go"), []byte("package book\n\nfunc broken( {\n"), 0o644)
		},
		"file cut partway": func(dir string) {
			os.WriteFile(filepath.Join(dir, "main.go"), mains[:len(mains)/2], 0o644)
		},
		"empty main file": func(dir string) {
			os.WriteFile(filepath.Join(dir, "main.go"), nil, 0o644)
		},
		"type error": func(dir string) {
			os.WriteFile(filepath.Join(dir, "main.go"), append(mains, []byte("\nfunc extra() { undefinedThing() }\n")...), 0o644)
		},
		"no go files": func(dir string) {
			for _, f := range []string{"main.go", "main_test.go"} {
				os.Remove(filepath.Join(dir, f))
			}
		},
		"other program": func(dir string) {
			os.RemoveAll(filepath.Join(dir, "book"))
			os.RemoveAll(filepath.Join(dir, "parse"))
			os.Remove(filepath.Join(dir, "main_test.go"))
			os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/other\n\ngo 1.22\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(hello()) }\n\nfunc hello() string { return fmt.Sprint(\"hi\") }\n"), 0o644)
		},
		"no go.mod": func(dir string) {
			os.Remove(filepath.Join(dir, "go.mod"))
		},
	}
	for name, breakIt := range cases {
		dir := copyTree(t, v1)
		breakIt(dir)
		for _, extra := range [][]string{nil, {"--fail"}} {
			args := append([]string{"check", "-C", dir, "--profile", abs(t, profileV1)}, extra...)
			out, errOut, code := pgostale(t, args...)
			if code != 2 || out != "" || !strings.Contains(errOut, "can't check") {
				t.Errorf("%s %v: exit %d, stdout %q, stderr %q", name, extra, code, out, errOut)
			}
		}
	}
}

func TestSaveKeepsCompilerOutput(t *testing.T) {
	save := filepath.Join(t.TempDir(), "build.log")
	if _, errOut, code := pgostale(t, "check", "-C", "../examples/ledger/v1", "--profile", abs(t, profileV1), "--save", save); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	b, err := os.ReadFile(save)
	if err != nil || !bytes.Contains(b, []byte(`{"Pkg":"main"`)) {
		t.Errorf("saved log lacks call site data (%v)", err)
	}
}

// TestOtherArchitecture builds for an architecture other than the one
// running the test. Matching happens before code generation, so the
// result must be the same.
func TestOtherArchitecture(t *testing.T) {
	other := "amd64"
	if runtime.GOARCH == "amd64" {
		other = "arm64"
	}
	native, _, code := check(t, "../examples/ledger/v2", profileV1)
	if code != 0 {
		t.Fatalf("native: exit %d", code)
	}
	cmd := exec.Command(bin, "check", "-C", "../examples/ledger/v2", "--profile", abs(t, profileV1), "--json")
	cmd.Env = append(os.Environ(), "GOARCH="+other, "GOOS=linux")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("GOARCH=%s: %v", other, err)
	}
	var cross report
	if err := json.Unmarshal(out, &cross); err != nil {
		t.Fatal(err)
	}
	if cross.Score != native.Score || len(cross.Lost) != len(native.Lost) || len(cross.Missing) != len(native.Missing) {
		t.Errorf("GOOS=linux GOARCH=%s: score %v, %d lost, %d missing; native: %v, %d, %d",
			other, cross.Score, len(cross.Lost), len(cross.Missing), native.Score, len(native.Lost), len(native.Missing))
	}
}
