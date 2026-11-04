package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
)

var realProfiles = []string{
	"../../examples/profiles/ledger-v1.pprof",
	"../../examples/profiles/ledger-v1-test.pprof",
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustProfile(t *testing.T, path string) *EdgeProfile {
	t.Helper()
	ep, err := ReadProfile(readFile(t, path))
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return ep
}

// parsePreprofile reads `go tool preprofile` output, for cross-checking.
func parsePreprofile(t *testing.T, data []byte) []Edge {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if lines[0] != preprofileHeader || (len(lines)-1)%3 != 0 {
		t.Fatalf("unexpected preprofile output:\n%s", data)
	}
	var edges []Edge
	for i := 1; i < len(lines); i += 3 {
		f := strings.Fields(lines[i+2])
		off, _ := strconv.Atoi(f[0])
		w, _ := strconv.ParseInt(f[1], 10, 64)
		edges = append(edges, Edge{Caller: lines[i], Callee: lines[i+1], Offset: off, Weight: w})
	}
	return edges
}

func edgeSet(edges []Edge) map[Edge]bool {
	m := map[Edge]bool{}
	for _, e := range edges {
		m[e] = true
	}
	return m
}

// TestEdgesMatchPreprofile checks the pprof reader against the Go
// toolchain's own reduction of the same profile (Go 1.23 and newer).
func TestEdgesMatchPreprofile(t *testing.T) {
	if err := exec.Command("go", "tool", "-n", "preprofile").Run(); err != nil {
		t.Skip("go tool preprofile needs Go 1.23 or newer")
	}
	profiles := append([]string{"../../examples/real/gojq/gojq-v0.12.17.pprof"}, realProfiles...)
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		p := filepath.Join(strings.TrimSpace(string(out)), "src", "cmd", "compile", "default.pgo")
		if _, err := os.Stat(p); err == nil {
			profiles = append(profiles, p)
		}
	}
	for _, p := range profiles {
		out := filepath.Join(t.TempDir(), "pre.txt")
		if b, err := exec.Command("go", "tool", "preprofile", "-i", p, "-o", out).CombinedOutput(); err != nil {
			t.Fatalf("preprofile %s: %v\n%s", p, err, b)
		}
		want := parsePreprofile(t, readFile(t, out))
		got := mustProfile(t, p)
		if !reflect.DeepEqual(edgeSet(got.Edges), edgeSet(want)) || len(got.Edges) != len(want) {
			t.Errorf("%s: %d edges, preprofile has %d, or they differ", p, len(got.Edges), len(want))
		}
		var total int64
		for _, e := range want {
			total += e.Weight
		}
		if got.Total != total {
			t.Errorf("%s: total %d, preprofile %d", p, got.Total, total)
		}
		// Same order as the compiler sorts them.
		for i := range want {
			if got.Edges[i] != want[i] {
				t.Errorf("%s: edge %d is %v, preprofile has %v", p, i, got.Edges[i], want[i])
				break
			}
		}
	}
}

func TestReadProfileRealExample(t *testing.T) {
	ep := mustProfile(t, realProfiles[0])
	if ep.Total < 1000 || len(ep.Edges) < 50 {
		t.Fatalf("total %d, %d edges", ep.Total, len(ep.Edges))
	}
	found := false
	for _, e := range ep.Edges {
		if e.Caller == "main.Digest" && e.Callee == "hash/fnv.(*sum64a).Write" && e.Offset == 3 {
			found = true
		}
	}
	if !found {
		t.Error("missing main.Digest +3 -> hash/fnv.(*sum64a).Write")
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// realProfileOf records a real profile of kind ("heap", "goroutine") from
// this test process.
func realProfileOf(t *testing.T, kind string) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup(kind).WriteTo(&buf, 0); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// emptyCPUProfile records a real CPU profile of an idle moment.
func emptyCPUProfile(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.StartCPUProfile(&buf); err != nil {
		t.Skip("CPU profiling is busy:", err)
	}
	pprof.StopCPUProfile()
	return buf.Bytes()
}

func TestReadProfileRejectsBadInput(t *testing.T) {
	real := readFile(t, realProfiles[0])
	raw := gunzip(t, real)
	pre := []byte(preprofileHeader + "\nmain.main\nmain.work\n3 100\n")
	rng := rand.New(rand.NewSource(1))
	random := make([]byte, 4096)
	rng.Read(random)
	cases := map[string][]byte{
		"empty":             {},
		"whitespace":        []byte(" \n\t\n"),
		"random bytes":      random,
		"random gzip":       gzipBytes(random),
		"text":              []byte("hello, this is not a profile\n"),
		"failed build":      []byte("# example.com/ledger\n./main.go:12:2: undefined: Generate\n"),
		"failed run":        []byte("panic: runtime error: index out of range [1] with length 1\n\ngoroutine 1 [running]:\nmain.main()\n\t/tmp/x/main.go:9 +0x1d\nexit status 2\n"),
		"gzip header only":  real[:10],
		"gzip cut in half":  real[:len(real)/2],
		"gzip cut near end": real[:len(real)-3],
		"raw cut in half":   raw[:len(raw)/2],
		"heap profile":      realProfileOf(t, "heap"),
		"goroutine profile": realProfileOf(t, "goroutine"),
		"preprofile text":   pre,
		"idle cpu profile":  emptyCPUProfile(t),
	}
	for name, data := range cases {
		ep, err := ReadProfile(data)
		if err == nil {
			t.Errorf("%s: accepted, %d edges", name, len(ep.Edges))
		}
	}
}

// TestTruncatedProfiles cuts a real profile at every length. Each cut must
// be refused, or give exactly the edges of the whole file (a raw profile
// cut after its last field the compiler reads).
func TestTruncatedProfiles(t *testing.T) {
	for _, p := range realProfiles {
		real := readFile(t, p)
		full := mustProfile(t, p)
		for n := 0; n < len(real); n++ {
			if ep, err := ReadProfile(real[:n]); err == nil {
				t.Fatalf("%s: gzip cut at %d of %d accepted (%d edges)", p, n, len(real), len(ep.Edges))
			}
		}
		raw := gunzip(t, real)
		for n := 0; n < len(raw); n++ {
			ep, err := ReadProfile(raw[:n])
			if err != nil {
				continue
			}
			if !reflect.DeepEqual(ep.Edges, full.Edges) {
				t.Fatalf("%s: raw cut at %d of %d accepted with different edges", p, n, len(raw))
			}
		}
	}
}

func TestHotEdges(t *testing.T) {
	ep := &EdgeProfile{Total: 100, Edges: []Edge{{Caller: "a", Weight: 60}, {Caller: "b", Weight: 39}, {Caller: "c", Weight: 1}}}
	// 60 + 39 = 99 is not above 99%, so the third edge is hot too.
	if got := len(ep.HotEdges()); got != 3 {
		t.Errorf("got %d hot edges, want 3", got)
	}
	ep = &EdgeProfile{Total: 1000, Edges: []Edge{{Caller: "a", Weight: 995}, {Caller: "b", Weight: 4}, {Caller: "c", Weight: 1}}}
	if got := len(ep.HotEdges()); got != 1 {
		t.Errorf("got %d hot edges, want 1", got)
	}
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"pkg.Map[go.shape.struct { A int },go.shape.int64]": "pkg.Map[...]",
		"pkg.(*List[go.shape.int]).Push":                    "pkg.(*List[...]).Push",
		"pkg.F":                                             "pkg.F",
	} {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"hash/fnv.(*sum64a).Write":         "Write",
		"example.com/a/b.Map[go.shape]":    "Map",
		"main.main":                        "main",
		"gopkg.in/yaml.v3.(*parser).parse": "parse",
	} {
		if got := baseName(in); got != want {
			t.Errorf("baseName(%q) = %q, want %q", in, got, want)
		}
	}
	for name, want := range map[string]bool{
		"runtime.mallocgc":                         true,
		"runtime.growslice":                        true,
		"runtime.(*mspan).init":                    true,
		"internal/runtime/maps.runtime_mapaccess1": true,
		"gcWriteBarrier":                           true,
		"memeqbody":                                true,
		"runtime.GC":                               false,
		"runtime.(*Frames).Next":                   false,
		"strings.ToUpper":                          false,
	} {
		if got := compilerInserted(name); got != want {
			t.Errorf("compilerInserted(%q) = %v", name, got)
		}
	}
	for _, c := range []struct {
		pos       string
		file      string
		line, col int
		ok        bool
	}{
		{"/a/b.go:12:7", "/a/b.go", 12, 7, true},
		{"/a/b.go:18190", "/a/b.go", 18190, 0, true},
		{`C:\a\b.go:3:4`, `C:\a\b.go`, 3, 4, true},
		{"nonsense", "", 0, 0, false},
		{"/a/b.go:x:y", "", 0, 0, false},
	} {
		f, l, col, ok := posLineCol(c.pos)
		if f != c.file || l != c.line || col != c.col || ok != c.ok {
			t.Errorf("posLineCol(%q) = %q %d %d %v", c.pos, f, l, col, ok)
		}
	}
	for in, want := range map[string]string{
		"\t\tt += hash":      "hash",
		"\th.Write":          "Write",
		"x := book.Map[int]": "Map",
		"f(a)[0]":            "",
		"":                   "",
	} {
		got, _ := identBefore(in)
		if got != want {
			t.Errorf("identBefore(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFuncLinkNames(t *testing.T) {
	dir := t.TempDir()
	src := `package p

func F() {}

func G[T any](x T) {}

type T struct{}

func (T) M() {}

func (t *T) P() {
	defer t.unlock()
}

func (t *T) unlock() {}

type L[E any] struct{}

func (l *L[E]) Push(e E) {}
`
	file := filepath.Join(dir, "p.go")
	os.WriteFile(file, []byte(src), 0o644)
	fs := newFileSource("")
	for name, line := range map[string]int{
		"ex/p.F":              3,
		"ex/p.G[...]":         5,
		"ex/p.T.M":            9,
		"ex/p.(*T).P":         11,
		"ex/p.(*L[...]).Push": 19,
	} {
		got, ok := fs.FuncStart(file, "ex/p", name)
		if !ok || got != line {
			t.Errorf("FuncStart(%s) = %d %v, want %d", name, got, ok, line)
		}
	}
	if d := fs.Deferred(file, "ex/p", "ex/p.(*T).P"); !reflect.DeepEqual(d, []string{"unlock"}) {
		t.Errorf("Deferred = %v", d)
	}
}

// fakeSource serves call names and start lines for Match tests.
type fakeSource struct {
	names     map[string]string // "file:line:col" -> name
	starts    map[string]int
	deferred  map[string][]string
	funcLines map[int]bool
}

func (f fakeSource) CallName(file string, line, col int) (string, bool) {
	n, ok := f.names[fmt.Sprintf("%s:%d:%d", file, line, col)]
	return n, ok
}

func (f fakeSource) FuncStart(file, pkg, name string) (int, bool) {
	l, ok := f.starts[name]
	return l, ok
}

func (f fakeSource) Deferred(file, pkg, name string) []string { return f.deferred[name] }

func (f fakeSource) FuncLines(file string) map[int]bool { return f.funcLines }

func profileOf(edges ...Edge) *EdgeProfile {
	ep := &EdgeProfile{Format: "pprof"}
	for _, e := range edges {
		ep.Total += e.Weight
	}
	ep.Edges = edges
	sortEdges(ep.Edges)
	return ep
}

var testPkgs = []Package{
	{ImportPath: "ex.com/app", Name: "main", LinkPath: "main"},
	{ImportPath: "ex.com/app/lib", Name: "lib", LinkPath: "ex.com/app/lib"},
}

func site(caller, pos string, w int64) CallSite {
	return CallSite{Pkg: "main", Pos: pos, Caller: caller, Direct: true, Weight: w}
}

func TestMatchHealthy(t *testing.T) {
	ep := profileOf(
		Edge{Caller: "main.work", Offset: 3, Callee: "ex.com/app/lib.Hash", Weight: 50},
		Edge{Caller: "main.work", Offset: 3, Callee: "runtime.growslice", Weight: 5},
		Edge{Caller: "main.main", Offset: 2, Callee: "main.work", Weight: 40},
		Edge{Caller: "strings.ToUpper", Offset: 9, Callee: "unicode.ToUpper", Weight: 30},
	)
	sites := []CallSite{
		site("main.work", "/m.go:13:10", 55),
		site("main.main", "/m.go:22:6", 40),
		site("main.main", "/m.go:23:6", 0),
	}
	src := fakeSource{names: map[string]string{"/m.go:13:10": "Hash", "/m.go:22:6": "work", "/m.go:23:6": "other"},
		starts: map[string]int{"main.work": 10, "main.main": 20}}
	rep, err := Match(ep, sites, testPkgs, "ex.com/app", src)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Score != 100 || len(rep.Lost) != 0 || rep.Module != 90 || rep.Inserted != 5 || rep.Outside != 30 {
		t.Errorf("got %+v", rep)
	}
	// Empty lists are [] in JSON, not null, so scripts can count them.
	b, _ := json.Marshal(rep)
	if !bytes.Contains(b, []byte(`"lostEdges":[]`)) || !bytes.Contains(b, []byte(`"missingFunctions":[]`)) {
		t.Errorf("JSON: %s", b)
	}
}

func TestMatchFindsDrift(t *testing.T) {
	ep := profileOf(
		Edge{Caller: "main.work", Offset: 3, Callee: "ex.com/app/lib.Hash", Weight: 50},  // moved
		Edge{Caller: "main.work", Offset: 4, Callee: "ex.com/app/lib.Split", Weight: 20}, // line now calls Join
		Edge{Caller: "main.gone", Offset: 1, Callee: "ex.com/app/lib.Hash", Weight: 30},  // missing
		Edge{Caller: "main.main", Offset: 2, Callee: "main.work", Weight: 40},            // fine
	)
	sites := []CallSite{
		site("main.work", "/m.go:14:10", 20),
		site("main.work", "/m.go:16:10", 0),
		site("main.main", "/m.go:22:6", 40),
	}
	src := fakeSource{names: map[string]string{"/m.go:14:10": "Join", "/m.go:16:10": "Hash", "/m.go:22:6": "work"},
		starts: map[string]int{"main.work": 10, "main.main": 20}}
	rep, err := Match(ep, sites, testPkgs, "ex.com/app", src)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, l := range rep.Lost {
		reasons[l.Caller+"+"+strconv.Itoa(l.Offset)] = l.Reason + " " + l.Where + " " + l.Now
	}
	want := map[string]string{
		"main.work+3": "call-site-moved /m.go:13 ",
		"main.work+4": "callee-changed /m.go:14 Join",
		"main.gone+1": "function-missing  ",
	}
	if !reflect.DeepEqual(reasons, want) {
		t.Errorf("lost = %v", reasons)
	}
	if rep.Matched != 40 || rep.Module != 140 {
		t.Errorf("matched %d of %d", rep.Matched, rep.Module)
	}
	if len(rep.Missing) != 1 || rep.Missing[0].Name != "main.gone" {
		t.Errorf("missing = %+v", rep.Missing)
	}
}

func TestMatchDeferredCallIsNotACalleeChange(t *testing.T) {
	// A deferred call runs at the return line, so the profile puts it there.
	ep := profileOf(Edge{Caller: "main.f", Offset: 4, Callee: "sync.(*Mutex).Unlock", Weight: 50})
	sites := []CallSite{site("main.f", "/m.go:14:12", 50)}
	src := fakeSource{names: map[string]string{"/m.go:14:12": "compute"},
		starts: map[string]int{"main.f": 10}, deferred: map[string][]string{"main.f": {"Unlock"}}}
	rep, err := Match(ep, sites, testPkgs, "ex.com/app", src)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Score != 100 {
		t.Errorf("score %v, lost %+v", rep.Score, rep.Lost)
	}
}

func TestMatchSolvesStartWithoutSource(t *testing.T) {
	ep := profileOf(
		Edge{Caller: "main.f.func1", Offset: 2, Callee: "ex.com/app/lib.A", Weight: 30},
		Edge{Caller: "main.f.func1", Offset: 5, Callee: "ex.com/app/lib.B", Weight: 20},
	)
	sites := []CallSite{site("main.f.func1", "/m.go:42:3", 30), site("main.f.func1", "/m.go:44:3", 0)}
	rep, err := Match(ep, sites, testPkgs, "ex.com/app", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Lost) != 1 || rep.Lost[0].Where != "/m.go:45" || rep.Matched != 30 {
		t.Errorf("got %+v", rep.Lost)
	}
}

func TestMissingHints(t *testing.T) {
	cases := []struct {
		missing string
		have    string
		want    string
	}{
		{"ex.com/app.work", "main.work", "go test binary"},
		{"ex.com/app.BenchmarkRun", "main.main", "a test function"},
		{"ex.com/app/lib.T.M", "ex.com/app/lib.(*T).M", "receiver changed"},
		{"main.Digest", "ex.com/app/lib.Digest", "moved to another package: ex.com/app/lib.Digest"},
		{"main.run.func1", "main.run.func2", "a closure"},
		{"ex.com/app/lib.free", "ex.com/app/lib.(*S).free", "probably now ex.com/app/lib.(*S).free"},
		{"ex.com/app/lib.Switch", "ex.com/app/lib.Switch[go.shape.int]", "now a generic function"},
		{"ex.com/app/lib.gone", "ex.com/app/lib.other", "renamed, removed, or has no calls left"},
	}
	for _, c := range cases {
		ep := profileOf(Edge{Caller: c.missing, Offset: 1, Callee: "strings.ToUpper", Weight: 100})
		pkg := "main"
		if strings.HasPrefix(c.have, "ex.com/app/lib") {
			pkg = "ex.com/app/lib"
		}
		sites := []CallSite{{Pkg: pkg, Pos: "/x.go:5:5", Caller: c.have, Direct: true}}
		// One edge that still matches, so the report isn't refused.
		ep.Edges = append(ep.Edges, Edge{Caller: "main.keep", Offset: 1, Callee: "strings.ToLower", Weight: 10})
		ep.Total += 10
		sites = append(sites, CallSite{Pkg: "main", Pos: "/k.go:2:5", Caller: "main.keep", Direct: true, Weight: 10})
		rep, err := Match(ep, sites, testPkgs, "ex.com/app", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Missing) != 1 || !strings.Contains(rep.Missing[0].Hint, c.want) {
			t.Errorf("%s with %s: %+v, want hint %q", c.missing, c.have, rep.Missing, c.want)
		}
		// With nothing left that matches, it refuses and passes the hint on.
		_, err = Match(profileOf(Edge{Caller: c.missing, Offset: 1, Callee: "strings.ToUpper", Weight: 100}), sites[:1], testPkgs, "ex.com/app", nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s alone: %v", c.missing, err)
		}
	}
}

func TestMatchRenameGuess(t *testing.T) {
	ep := profileOf(
		Edge{Caller: "ex.com/app/lib.ParseLine", Offset: 1, Callee: "strings.Split", Weight: 30},
		Edge{Caller: "ex.com/app/lib.ParseLine", Offset: 5, Callee: "strconv.ParseInt", Weight: 20},
	)
	sites := []CallSite{
		{Pkg: "ex.com/app/lib", Pos: "/l.go:21:20", Caller: "ex.com/app/lib.Line", Direct: true},
		{Pkg: "ex.com/app/lib", Pos: "/l.go:25:20", Caller: "ex.com/app/lib.Line", Direct: true},
		{Pkg: "ex.com/app/lib", Pos: "/l.go:41:20", Caller: "ex.com/app/lib.Other", Direct: true},
	}
	src := fakeSource{names: map[string]string{"/l.go:21:20": "Split", "/l.go:25:20": "ParseInt", "/l.go:41:20": "Split"},
		starts: map[string]int{"ex.com/app/lib.Line": 20, "ex.com/app/lib.Other": 40}}
	ep.Edges = append(ep.Edges, Edge{Caller: "main.keep", Offset: 1, Callee: "strings.ToLower", Weight: 10})
	sites = append(sites, CallSite{Pkg: "main", Pos: "/k.go:2:5", Caller: "main.keep", Direct: true, Weight: 10})
	rep, err := Match(ep, sites, testPkgs, "ex.com/app", src)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Missing) != 1 || !strings.Contains(rep.Missing[0].Hint, "probably renamed to ex.com/app/lib.Line") {
		t.Errorf("got %+v", rep.Missing)
	}
}

func TestMatchRefusesWhatItCantCheck(t *testing.T) {
	ep := profileOf(Edge{Caller: "other.com/x.F", Offset: 1, Callee: "strings.ToUpper", Weight: 100})
	if _, err := Match(ep, []CallSite{site("main.main", "/m.go:3:3", 0)}, testPkgs, "ex.com/app", nil); err == nil {
		t.Error("a profile of another program was accepted")
	}
	ep = profileOf(Edge{Caller: "main.main", Offset: 1, Callee: "main.f", Weight: 100})
	if _, err := Match(ep, nil, testPkgs, "ex.com/app", nil); err == nil {
		t.Error("no compiler data was accepted")
	}
	if _, err := Match(ep, []CallSite{site("main.main", "garbage", 0)}, testPkgs, "ex.com/app", nil); err == nil {
		t.Error("a garbage position was accepted")
	}
}

func TestParseCallSites(t *testing.T) {
	out := []byte(`# example.com/x
./main.go:3:4: PGO devirtualize considering call f()
{"Pkg":"main","Pos":"/x/main.go:3:4","Caller":"main.main","Direct":true,"Interface":false,"Weight":7,"Hottest":"main.f","HottestWeight":7,"Devirtualized":"","DevirtualizedWeight":0}
hot-callsite-thres-from-CDF=0.5
`)
	sites, err := ParseCallSites(out)
	if err != nil || len(sites) != 1 || sites[0].Weight != 7 || !sites[0].Direct {
		t.Fatalf("got %+v, %v", sites, err)
	}
	if _, err := ParseCallSites([]byte(`{"Pkg":"main","Pos":`)); err == nil {
		t.Error("a cut-off JSON line was accepted")
	}
}

func TestRunUsage(t *testing.T) {
	var out, errb bytes.Buffer
	for _, args := range [][]string{nil, {"nope"}, {"check", "--min", "120"}, {"check", "a", "b"}, {"edges"}, {"edges", "/does/not/exist"}} {
		if code := run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	out.Reset()
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), version) {
		t.Errorf("version: %d %q", code, out.String())
	}
}

func TestEdgesCommand(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"edges", "--top", "3", realProfiles[0]}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], "call edges") {
		t.Errorf("got:\n%s", out.String())
	}
	dir := t.TempDir()
	for name, data := range map[string][]byte{"empty": nil, "garbage": []byte("garbage\n"), "cut": readFile(t, realProfiles[0])[:200]} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, data, 0o644)
		out.Reset()
		if code := run([]string{"edges", p}, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("%s: exit %d, output %q", name, code, out.String())
		}
	}
}

// TestMatchPicksTheRealClosureStart is the case found in gojq: a closure
// whose call at +2 and compiler-inserted call at +3 have the same weight,
// so line 39 fits a start at 37 or 36. Only 37 has a func keyword.
func TestMatchPicksTheRealClosureStart(t *testing.T) {
	ep := profileOf(
		Edge{Caller: "main.Compare.func4", Offset: 2, Callee: "main.Compare", Weight: 1},
		Edge{Caller: "main.Compare.func4", Offset: 3, Callee: "runtime.convT64", Weight: 1},
		Edge{Caller: "main.main", Offset: 1, Callee: "main.Compare", Weight: 40},
	)
	sites := []CallSite{site("main.Compare.func4", "/c.go:39:22", 1), site("main.Compare.func4", "/c.go:43:21", 0), site("main.main", "/c.go:61:9", 40)}
	for name, src := range map[string]Source{
		"with source":    fakeSource{funcLines: map[int]bool{37: true, 60: true}, starts: map[string]int{"main.main": 60}},
		"without source": nil,
	} {
		rep, err := Match(ep, sites, testPkgs, "ex.com/app", src)
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Lost) != 0 || rep.Score != 100 {
			t.Errorf("%s: lost %+v", name, rep.Lost)
		}
	}
}
