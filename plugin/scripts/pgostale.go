// Command pgostale checks whether a Go PGO profile (default.pgo) still
// matches the code it is applied to.
//
// Go's profile-guided optimization finds hot call sites by function name and
// by line offset from the start of the function. After a rename, a moved
// function or a few inserted lines, the profile's call sites no longer match
// and the compiler silently skips them. pgostale builds the package with the
// profile, asks the compiler which call sites it matched
// (-gcflags=-d=pgodebug=3), and reports the share of the profile's hot call
// weight in your module that still applies, and what was lost.
//
// It uses only the standard library, so `go run` needs no setup.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const version = "0.1.0"

// hotCDFPercent is the compiler's default inlining hot call site threshold
// (cmd/compile/internal/inline: inlineCDFHotCallSiteThresholdPercent).
const hotCDFPercent = 99.0

// ---------------------------------------------------------------------------
// Profile reading

// Edge is one call edge as the compiler sees it: a call from Caller at
// Offset lines after Caller's first line, to Callee.
type Edge struct {
	Caller string `json:"caller"`
	Offset int    `json:"offset"`
	Callee string `json:"callee"`
	Weight int64  `json:"weight"`
}

// EdgeProfile is a profile reduced to the compiler's weighted call edges,
// sorted by weight, heaviest first.
type EdgeProfile struct {
	Format  string // "pprof"
	Samples int
	Total   int64
	Edges   []Edge
}

const preprofileHeader = "GO PREPROFILE V1"

// minSamples is the fewest samples in call edges that pgostale will judge.
// A CPU profile takes 100 samples a second, so this is 0.2 seconds of CPU.
const minSamples = 20

// ReadProfile parses a CPU profile in pprof format, gzipped or not. It
// returns an error for anything the compiler would ignore or reject, and
// for the go tool preprofile text format, so that an empty, cut off or
// wrong profile is never reported as matching.
func ReadProfile(data []byte) (*EdgeProfile, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("the profile file is empty")
	}
	if bytes.HasPrefix(data, []byte(preprofileHeader)) {
		return nil, errors.New("this is the text format of go tool preprofile, which has no end marker, so a cut-off file can't be detected; give the pprof file it was made from")
	}
	raw := data
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("not a readable gzip file: %v", err)
		}
		raw, err = io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("the gzip data is cut off or damaged (%v); was the profile still being written?", err)
		}
	}
	p, err := decodePprof(raw)
	if err != nil {
		return nil, fmt.Errorf("not a pprof profile: %v", err)
	}
	return p.edges()
}

func sortEdges(edges []Edge) {
	// Same order as cmd/internal/pgo sortByWeight.
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.Caller != b.Caller {
			return a.Caller < b.Caller
		}
		if a.Callee != b.Callee {
			return a.Callee < b.Callee
		}
		return a.Offset < b.Offset
	})
}

// HotEdges returns the edges the compiler treats as hot call site
// candidates: the heaviest edges until their cumulative weight passes 99%
// of the total, including the edge that crosses it.
func (ep *EdgeProfile) HotEdges() []Edge {
	var cum int64
	for i, e := range ep.Edges {
		cum += e.Weight
		if float64(cum)/float64(ep.Total)*100 > hotCDFPercent {
			return ep.Edges[:i+1]
		}
	}
	return ep.Edges
}

// pprofData is the part of a pprof protobuf message the compiler uses.
type pprofData struct {
	sampleTypes [][2]int64 // string indexes of type and unit
	samples     []ppSample
	locations   map[uint64]*ppLocation
	functions   map[uint64]*ppFunction
	strings     []string
}

type ppSample struct {
	locs   []uint64
	values []int64
}

type ppLocation struct {
	id      uint64
	address uint64
	lines   []ppLine
}

type ppLine struct {
	function uint64
	line     int64
}

type ppFunction struct {
	id        uint64
	name      int64
	startLine int64
}

type pbReader struct {
	b []byte
}

func (r *pbReader) varint() (uint64, error) {
	var x uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if len(r.b) == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		c := r.b[0]
		r.b = r.b[1:]
		x |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return x, nil
		}
	}
	return 0, errors.New("varint too long")
}

// field reads one field and returns its number, wire type, varint value
// (wire type 0) or bytes (wire type 2).
func (r *pbReader) field() (num int, wire int, v uint64, data []byte, err error) {
	key, err := r.varint()
	if err != nil {
		return 0, 0, 0, nil, err
	}
	num, wire = int(key>>3), int(key&7)
	if num == 0 {
		return 0, 0, 0, nil, errors.New("field number 0")
	}
	switch wire {
	case 0:
		v, err = r.varint()
	case 1:
		if len(r.b) < 8 {
			return 0, 0, 0, nil, io.ErrUnexpectedEOF
		}
		r.b = r.b[8:]
	case 2:
		var n uint64
		n, err = r.varint()
		if err == nil {
			if n > uint64(len(r.b)) {
				return 0, 0, 0, nil, io.ErrUnexpectedEOF
			}
			data, r.b = r.b[:n], r.b[n:]
		}
	case 5:
		if len(r.b) < 4 {
			return 0, 0, 0, nil, io.ErrUnexpectedEOF
		}
		r.b = r.b[4:]
	default:
		return 0, 0, 0, nil, fmt.Errorf("unknown wire type %d", wire)
	}
	return num, wire, v, data, err
}

// uints reads a repeated integer field, packed or not.
func uints(wire int, v uint64, data []byte, dst *[]uint64) error {
	if wire == 0 {
		*dst = append(*dst, v)
		return nil
	}
	if wire != 2 {
		return fmt.Errorf("bad wire type %d for an integer list", wire)
	}
	r := pbReader{data}
	for len(r.b) > 0 {
		x, err := r.varint()
		if err != nil {
			return err
		}
		*dst = append(*dst, x)
	}
	return nil
}

func decodePprof(b []byte) (*pprofData, error) {
	p := &pprofData{locations: map[uint64]*ppLocation{}, functions: map[uint64]*ppFunction{}}
	r := pbReader{b}
	for len(r.b) > 0 {
		num, wire, _, data, err := r.field()
		if err != nil {
			return nil, err
		}
		if num <= 6 && wire != 2 {
			return nil, fmt.Errorf("field %d has wire type %d", num, wire)
		}
		switch num {
		case 1: // sample_type
			var vt [2]int64
			m := pbReader{data}
			for len(m.b) > 0 {
				n, _, v, _, err := m.field()
				if err != nil {
					return nil, err
				}
				if n == 1 || n == 2 {
					vt[n-1] = int64(v)
				}
			}
			p.sampleTypes = append(p.sampleTypes, vt)
		case 2: // sample
			var s ppSample
			m := pbReader{data}
			for len(m.b) > 0 {
				n, w, v, d, err := m.field()
				if err != nil {
					return nil, err
				}
				switch n {
				case 1:
					if err := uints(w, v, d, &s.locs); err != nil {
						return nil, err
					}
				case 2:
					var vals []uint64
					if err := uints(w, v, d, &vals); err != nil {
						return nil, err
					}
					for _, x := range vals {
						s.values = append(s.values, int64(x))
					}
				}
			}
			p.samples = append(p.samples, s)
		case 4: // location
			l := &ppLocation{}
			m := pbReader{data}
			for len(m.b) > 0 {
				n, _, v, d, err := m.field()
				if err != nil {
					return nil, err
				}
				switch n {
				case 1:
					l.id = v
				case 3:
					l.address = v
				case 4:
					var ln ppLine
					lm := pbReader{d}
					for len(lm.b) > 0 {
						n2, _, v2, _, err := lm.field()
						if err != nil {
							return nil, err
						}
						switch n2 {
						case 1:
							ln.function = v2
						case 2:
							ln.line = int64(v2)
						}
					}
					l.lines = append(l.lines, ln)
				}
			}
			if l.id == 0 {
				return nil, errors.New("location with id 0")
			}
			p.locations[l.id] = l
		case 5: // function
			f := &ppFunction{}
			m := pbReader{data}
			for len(m.b) > 0 {
				n, _, v, _, err := m.field()
				if err != nil {
					return nil, err
				}
				switch n {
				case 1:
					f.id = v
				case 2:
					f.name = int64(v)
				case 5:
					f.startLine = int64(v)
				}
			}
			if f.id == 0 {
				return nil, errors.New("function with id 0")
			}
			p.functions[f.id] = f
		case 6: // string_table
			p.strings = append(p.strings, string(data))
		}
	}
	if len(p.strings) == 0 || p.strings[0] != "" {
		return nil, errors.New("missing or bad string table")
	}
	str := func(i int64) (string, error) {
		if i < 0 || i >= int64(len(p.strings)) {
			return "", fmt.Errorf("string index %d out of range (%d strings); the file is probably cut off", i, len(p.strings))
		}
		return p.strings[i], nil
	}
	for _, vt := range p.sampleTypes {
		for _, i := range vt {
			if _, err := str(i); err != nil {
				return nil, err
			}
		}
	}
	for _, f := range p.functions {
		if _, err := str(f.name); err != nil {
			return nil, err
		}
	}
	for _, s := range p.samples {
		if len(s.values) != len(p.sampleTypes) {
			return nil, fmt.Errorf("a sample has %d values for %d sample types", len(s.values), len(p.sampleTypes))
		}
		for _, id := range s.locs {
			if p.locations[id] == nil {
				return nil, fmt.Errorf("a sample uses location %d, which is missing; the file is probably cut off", id)
			}
		}
	}
	for _, l := range p.locations {
		for _, ln := range l.lines {
			if ln.function != 0 && p.functions[ln.function] == nil {
				return nil, fmt.Errorf("location %d uses function %d, which is missing", l.id, ln.function)
			}
		}
	}
	return p, nil
}

type nodeKey struct {
	name      string
	address   uint64
	startLine int64
	line      int64
}

// edges reduces the profile to named call edges the way the compiler does
// (cmd/internal/pgo FromPProf with internal/profile NewGraph): only the
// last two locations of each stack count, inline frames expanded.
func (p *pprofData) edges() (*EdgeProfile, error) {
	idx := -1
	var kinds []string
	for i, vt := range p.sampleTypes {
		t, u := p.strings[vt[0]], p.strings[vt[1]]
		kinds = append(kinds, t+"/"+u)
		if (t == "samples" && u == "count") || (t == "cpu" && u == "nanoseconds") {
			idx = i
			break
		}
	}
	if idx < 0 {
		if len(kinds) == 0 {
			return nil, errors.New("the profile has no sample types")
		}
		return nil, fmt.Errorf("not a CPU profile: its sample types are %s, and PGO needs samples/count or cpu/nanoseconds", strings.Join(kinds, ", "))
	}
	if len(p.samples) == 0 {
		return nil, errors.New("the profile has no samples, so PGO does nothing with it; was the program idle, or stopped before the profile was written?")
	}
	nodes := map[*ppLocation][]nodeKey{}
	seenStart := false
	locNodes := func(l *ppLocation) []nodeKey {
		if ks, ok := nodes[l]; ok {
			return ks
		}
		var ks []nodeKey
		if len(l.lines) == 0 {
			ks = []nodeKey{{address: l.address}}
		}
		for _, ln := range l.lines {
			k := nodeKey{address: l.address, line: ln.line}
			if f := p.functions[ln.function]; f != nil {
				k.name = p.strings[f.name]
				k.startLine = f.startLine
			}
			ks = append(ks, k)
		}
		nodes[l] = ks
		return ks
	}
	weights := map[Edge]int64{}
	var total int64
	type pair struct{ from, to nodeKey }
	for _, s := range p.samples {
		w := s.values[idx]
		if w == 0 {
			continue
		}
		seenNode := map[nodeKey]bool{}
		seenEdge := map[pair]bool{}
		var parent *nodeKey
		i := 1
		if last := len(s.locs) - 1; last < i {
			i = last
		}
		for ; i >= 0; i-- {
			ks := locNodes(p.locations[s.locs[i]])
			for ni := len(ks) - 1; ni >= 0; ni-- {
				n := ks[ni]
				if n.startLine != 0 {
					seenStart = true
				}
				saw := seenNode[n]
				seenNode[n] = true
				if parent != nil && n != *parent {
					pr := pair{*parent, n}
					if !saw || !seenEdge[pr] {
						seenEdge[pr] = true
						e := Edge{Caller: parent.name, Offset: int(parent.line - parent.startLine), Callee: n.name}
						weights[e] += w
						total += w
					}
				}
				nn := n
				parent = &nn
			}
		}
	}
	for _, l := range p.locations {
		for _, k := range locNodes(l) {
			if k.startLine != 0 {
				seenStart = true
			}
		}
	}
	if total == 0 {
		return nil, errors.New("the profile has no call edges with weight, so PGO does nothing with it")
	}
	if p.strings[p.sampleTypes[idx][0]] == "samples" && total < minSamples {
		return nil, fmt.Errorf("the profile has only %d samples in call edges, too few to judge; did the program stop early?", total)
	}
	if !seenStart {
		return nil, errors.New("the profile has no function start lines, which Go 1.20 and newer write; the compiler rejects it")
	}
	ep := &EdgeProfile{Format: "pprof", Samples: len(p.samples), Total: total}
	for e, w := range weights {
		e.Weight = w
		ep.Edges = append(ep.Edges, e)
	}
	sortEdges(ep.Edges)
	return ep, nil
}

// ---------------------------------------------------------------------------
// Compiler data

// CallSite is one line of the compiler's -d=pgodebug=3 JSON output
// (cmd/compile/internal/devirtualize CallStat).
type CallSite struct {
	Pkg       string
	Pos       string
	Caller    string
	Direct    bool
	Interface bool
	Weight    int64
	Hottest   string
}

// ParseCallSites reads the compiler's JSON call site lines from build
// output and ignores everything else.
func ParseCallSites(out []byte) ([]CallSite, error) {
	var sites []CallSite
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte(`{"Pkg":`)) {
			continue
		}
		var c CallSite
		if err := json.Unmarshal(line, &c); err != nil {
			return nil, fmt.Errorf("unreadable compiler line %q: %v", line, err)
		}
		if c.Caller == "" || c.Pos == "" {
			return nil, fmt.Errorf("compiler line without caller or position: %q", line)
		}
		sites = append(sites, c)
	}
	return sites, sc.Err()
}

// posLineCol splits "file:line:col", or "file:line", which the compiler
// prints when it has no column (col is then 0).
func posLineCol(pos string) (string, int, int, bool) {
	i := strings.LastIndex(pos, ":")
	if i < 0 {
		return "", 0, 0, false
	}
	last, err := strconv.Atoi(pos[i+1:])
	if err != nil || last <= 0 {
		return "", 0, 0, false
	}
	if j := strings.LastIndex(pos[:i], ":"); j >= 0 {
		if line, err := strconv.Atoi(pos[j+1 : i]); err == nil && line > 0 {
			return pos[:j], line, last, true
		}
	}
	return pos[:i], last, 0, true
}

// normalizeName replaces type arguments with "...", so the compiler's
// F[go.shape.int] and the profile's F[...] compare equal.
func normalizeName(s string) string {
	if !strings.Contains(s, "[") {
		return s
	}
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '[':
			if depth == 0 {
				b.WriteString("[...]")
			}
			depth++
		case r == ']':
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// compilerInserted reports whether callee is a runtime helper that the
// compiler inserts (for make, append, map access, write barriers and so on).
// Such edges have no call expression in the source, so they never match.
func compilerInserted(callee string) bool {
	var rest string
	switch {
	case !strings.Contains(callee, "."):
		// Assembly helpers such as gcWriteBarrier, memeqbody and cmpbody,
		// reached through write barriers and string comparisons.
		return true
	case strings.HasPrefix(callee, "runtime."):
		rest = strings.TrimPrefix(callee, "runtime.")
	case strings.HasPrefix(callee, "internal/runtime/"):
		return true
	default:
		return false
	}
	rest = strings.TrimPrefix(rest, "(*")
	if rest == "" {
		return true
	}
	c := rest[0]
	return !(c >= 'A' && c <= 'Z')
}

// ---------------------------------------------------------------------------
// Matching

// Package is one package of the module being checked.
type Package struct {
	ImportPath string
	Name       string
	LinkPath   string // "main" for the main package, else ImportPath
}

// LostEdge is a profile call edge in the module that no longer matches.
type LostEdge struct {
	Edge
	Hot    bool   `json:"hot"`
	Reason string `json:"reason"` // "function-missing", "call-site-moved" or "callee-changed"
	Where  string `json:"where,omitempty"`
	Now    string `json:"now,omitempty"` // what the line calls now
}

// MissingFunc is a caller in the profile that the build doesn't have, or
// that has no calls left.
type MissingFunc struct {
	Name     string `json:"name"`
	Weight   int64  `json:"weight"`
	HotEdges int    `json:"hotEdges"`
	Hint     string `json:"hint"`
}

// Report is the result of a check.
type Report struct {
	Tool       string        `json:"tool"`
	GoVersion  string        `json:"goVersion,omitempty"`
	Package    string        `json:"package,omitempty"`
	Profile    string        `json:"profile,omitempty"`
	Format     string        `json:"profileFormat"`
	Edges      int           `json:"profileEdges"`
	Total      int64         `json:"profileWeight"`
	HotTotal   int           `json:"profileHotEdges"`
	Module     int64         `json:"moduleWeight"`
	Inserted   int64         `json:"compilerInsertedWeight"`
	Outside    int64         `json:"outsideWeight"`
	Matched    int64         `json:"matchedWeight"`
	HotEdges   int           `json:"moduleHotEdges"`
	HotMatched int           `json:"moduleHotEdgesMatched"`
	HotWeight  int64         `json:"moduleHotWeight"`
	HotMatchW  int64         `json:"moduleHotWeightMatched"`
	Score      float64       `json:"score"`
	ScoreBasis string        `json:"scoreBasis"`
	Min        float64       `json:"min"`
	Below      bool          `json:"belowMin"`
	Missing    []MissingFunc `json:"missingFunctions"`
	Lost       []LostEdge    `json:"lostEdges"`
	Unresolved []string      `json:"unresolvedCallers,omitempty"`
}

// closureRE matches compiler-generated function names: closures, defer
// and go wrappers, and range-over-func bodies.
var closureRE = regexp.MustCompile(`(\.func\d+|\.deferwrap\d+|\.gowrap\d+|-range\d+)(\.\d+|-range\d+)*$`)

// fnSites is the compiler's view of one function.
type fnSites struct {
	pkg   string // link path of its package
	file  string
	lines map[int]int64      // line -> profile weight the compiler matched
	calls map[int][]CallSite // line -> calls on that line
}

// Source answers questions about the source files the compiler read.
type Source interface {
	// CallName returns the name called at the position of a call's
	// opening parenthesis, such as "Write" for h.Write(b).
	CallName(file string, line, col int) (string, bool)
	// FuncStart returns the line of the func keyword of a named function
	// or method, by its normalized link name.
	FuncStart(file, pkgLink, name string) (int, bool)
	// Deferred returns the names the function calls with defer. A
	// deferred call runs at a return line, so the profile shows it there.
	Deferred(file, pkgLink, name string) []string
	// FuncLines returns the lines where a function, a function literal or
	// a range loop starts, or nil if the file can't be read.
	FuncLines(file string) map[int]bool
}

// baseName returns the last part of a function name: "Write" for
// "hash/fnv.(*sum64a).Write", "Map" for "pkg.Map[...]".
func baseName(fn string) string {
	fn = normalizeName(fn)
	fn = strings.TrimSuffix(fn, "[...]")
	if i := strings.LastIndex(fn, "/"); i >= 0 {
		fn = fn[i+1:]
	}
	if i := strings.LastIndex(fn, "."); i >= 0 {
		fn = fn[i+1:]
	}
	return fn
}

// namesAt returns the names called on a line, and whether every call there
// could be named. Calls of function values can't be.
func namesAt(src Source, fs *fnSites, line int) ([]string, bool) {
	var names []string
	all := true
	for _, c := range fs.calls[line] {
		if !c.Direct && !c.Interface {
			all = false
			continue
		}
		_, _, col, ok := posLineCol(c.Pos)
		if !ok || src == nil {
			all = false
			continue
		}
		n, ok := src.CallName(fs.file, line, col)
		if !ok {
			all = false
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, all
}

// calleeChecked reports whether the callee of e can be compared by name.
func calleeChecked(callee string) bool {
	return !closureRE.MatchString(normalizeName(callee))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Match compares profile edges with the compiler's call sites for the
// module's packages. src may be nil, which skips the callee name check.
func Match(ep *EdgeProfile, sites []CallSite, pkgs []Package, mainImportPath string, src Source) (*Report, error) {
	if len(sites) == 0 {
		return nil, errors.New("the compiler printed no PGO call site data (-d=pgodebug=3), so nothing can be checked; this Go version may print it differently")
	}
	rep := &Report{Tool: "pgostale " + version, Format: ep.Format, Edges: len(ep.Edges), Total: ep.Total,
		Missing: []MissingFunc{}, Lost: []LostEdge{}}
	hot := map[Edge]bool{}
	for _, e := range ep.HotEdges() {
		hot[e] = true
	}
	rep.HotTotal = len(hot)

	links := make([]string, 0, len(pkgs)+1)
	for _, p := range pkgs {
		links = append(links, p.LinkPath)
		if p.LinkPath == "main" && p.ImportPath != "main" {
			// A go test binary names package main by its import path.
			links = append(links, p.ImportPath)
		}
	}
	sort.Slice(links, func(i, j int) bool { return len(links[i]) > len(links[j]) })

	compiled := map[string]*fnSites{}
	for _, s := range sites {
		if strings.HasPrefix(s.Pos, "<autogenerated>") {
			continue // compiler-made wrappers have no source lines
		}
		file, line, _, ok := posLineCol(s.Pos)
		if !ok {
			return nil, fmt.Errorf("compiler call site with unreadable position %q", s.Pos)
		}
		name := normalizeName(s.Caller)
		fs := compiled[name]
		if fs == nil {
			fs = &fnSites{pkg: s.Pkg, file: file, lines: map[int]int64{}, calls: map[int][]CallSite{}}
			compiled[name] = fs
		}
		if file != fs.file {
			// Generic code from another file compiled into this package.
			continue
		}
		if s.Weight > fs.lines[line] {
			fs.lines[line] = s.Weight
		} else if _, ok := fs.lines[line]; !ok {
			fs.lines[line] = s.Weight
		}
		fs.calls[line] = append(fs.calls[line], s)
	}
	starts := map[string]int{}
	startOf := func(name string) (int, bool) {
		if s, ok := starts[name]; ok {
			return s, s > 0
		}
		fs := compiled[name]
		s := 0
		if fs != nil && src != nil {
			s, _ = src.FuncStart(fs.file, fs.pkg, name)
		}
		starts[name] = s
		return s, s > 0
	}

	type fnProfile struct {
		edges   []Edge
		offsets map[int]int64 // all edges, summed the way the compiler sums them
	}
	byCaller := map[string]*fnProfile{}
	var order []string
	for _, e := range ep.Edges {
		name := normalizeName(e.Caller)
		if pkg, _ := splitLink(name, links); pkg == "" {
			rep.Outside += e.Weight
			continue
		}
		fp := byCaller[name]
		if fp == nil {
			fp = &fnProfile{offsets: map[int]int64{}}
			byCaller[name] = fp
			order = append(order, name)
		}
		fp.offsets[e.Offset] += e.Weight
		if compilerInserted(e.Callee) {
			rep.Inserted += e.Weight
			continue
		}
		fp.edges = append(fp.edges, e)
		rep.Module += e.Weight
		if hot[e] {
			rep.HotEdges++
			rep.HotWeight += e.Weight
		}
	}
	if rep.Module == 0 {
		return nil, fmt.Errorf("none of the profile's %d call edges has a caller in this module's packages (%s), so it can't apply here: it is from another program, or the module path changed", len(ep.Edges), strings.Join(firstN(links, 5), ", "))
	}

	for _, name := range order {
		fp := byCaller[name]
		if len(fp.edges) == 0 {
			continue
		}
		fs := compiled[name]
		if fs == nil {
			mf := MissingFunc{Name: name}
			mf.Hint = missingHint(name, fp.edges, compiled, links, mainImportPath, src, startOf, func(c string) bool { return byCaller[c] != nil })
			for _, e := range fp.edges {
				mf.Weight += e.Weight
				le := LostEdge{Edge: e, Hot: hot[e], Reason: "function-missing"}
				if le.Hot {
					mf.HotEdges++
				}
				rep.Lost = append(rep.Lost, le)
			}
			rep.Missing = append(rep.Missing, mf)
			continue
		}
		var positive []int
		for l, w := range fs.lines {
			if w > 0 {
				positive = append(positive, l)
			}
		}
		sort.Ints(positive)
		// The function's first line: from the source if it agrees with
		// the compiler, else solved from the lines with weight.
		start, ok := startOf(name)
		if ok && !consistent(start, positive, fs.lines, fp.offsets) {
			ok = false
		}
		if !ok {
			var allowed map[int]bool
			if src != nil {
				allowed = src.FuncLines(fs.file)
			}
			eligible := map[int]int64{}
			for _, e := range fp.edges {
				eligible[e.Offset] += e.Weight
			}
			start, ok = solveStart(positive, fs.lines, fp.offsets, allowed, eligible)
			if !ok && len(allowed) > 0 {
				start, ok = solveStart(positive, fs.lines, fp.offsets, nil, eligible)
			}
		}
		if !ok && len(positive) > 0 {
			rep.Unresolved = append(rep.Unresolved, name)
		}
		for _, e := range fp.edges {
			le := LostEdge{Edge: e, Hot: hot[e], Reason: "call-site-moved"}
			matched := false
			if ok {
				line := start + e.Offset
				le.Where = fmt.Sprintf("%s:%d", fs.file, line)
				names, all := namesAt(src, fs, line)
				le.Now = strings.Join(names, ", ")
				if fs.lines[line] > 0 {
					matched = true
					checked := calleeChecked(e.Callee) && all && len(names) > 0
					if checked && src != nil && contains(src.Deferred(fs.file, fs.pkg, name), baseName(e.Callee)) {
						checked = false
					}
					if checked && !contains(names, baseName(e.Callee)) {
						matched = false
						le.Reason = "callee-changed"
					}
				}
			} else if len(positive) > 0 {
				// Fall back to the weights alone.
				for _, l := range positive {
					if fs.lines[l] == fp.offsets[e.Offset] {
						matched = true
					}
				}
			}
			if matched {
				rep.Matched += e.Weight
				if hot[e] {
					rep.HotMatched++
					rep.HotMatchW += e.Weight
				}
				continue
			}
			rep.Lost = append(rep.Lost, le)
		}
	}
	sort.SliceStable(rep.Missing, func(i, j int) bool { return rep.Missing[i].Weight > rep.Missing[j].Weight })
	if rep.Matched == 0 {
		why := ""
		if len(rep.Missing) > 0 {
			why = fmt.Sprintf(" (for example %s: %s)", rep.Missing[0].Name, rep.Missing[0].Hint)
		}
		return nil, fmt.Errorf("none of the profile's call edges in this module matches the code%s, so it is from another program or the code changed throughout; collect a new profile from the current build", why)
	}
	if rep.HotWeight > 0 {
		rep.Score = pct(rep.HotMatchW, rep.HotWeight)
		rep.ScoreBasis = "hot"
	} else {
		rep.Score = pct(rep.Matched, rep.Module)
		rep.ScoreBasis = "all"
	}
	sort.SliceStable(rep.Lost, func(i, j int) bool {
		if rep.Lost[i].Hot != rep.Lost[j].Hot {
			return rep.Lost[i].Hot
		}
		return rep.Lost[i].Weight > rep.Lost[j].Weight
	})
	return rep, nil
}

// consistent reports whether every line with matched weight sits at a
// profile offset from start with that same summed weight.
func consistent(start int, positive []int, lines map[int]int64, offsets map[int]int64) bool {
	for _, l := range positive {
		if offsets[l-start] != lines[l] {
			return false
		}
	}
	return true
}

// solveStart finds the first line of a function from the lines where the
// compiler matched profile weight. A start must explain every such line.
// If allowed is not empty, the start must be one of its lines (where a func
// keyword is). Among several starts, the one that matches the most weight
// of calls written in the source (eligible) wins.
func solveStart(positive []int, lines map[int]int64, offsets map[int]int64, allowed map[int]bool, eligible map[int]int64) (int, bool) {
	if len(positive) == 0 {
		return 0, false
	}
	l0 := positive[0]
	var cands []int
	for off, w := range offsets {
		s := l0 - off
		if w == lines[l0] && (len(allowed) == 0 || allowed[s]) {
			cands = append(cands, s)
		}
	}
	sort.Ints(cands)
	best, bestW, found := 0, int64(-1), false
	for _, s := range cands {
		if !consistent(s, positive, lines, offsets) {
			continue
		}
		var w int64
		for _, l := range positive {
			w += eligible[l-s]
		}
		if w > bestW {
			best, bestW, found = s, w, true
		}
	}
	return best, found
}

// splitLink splits a function name into its package link path and the rest.
func splitLink(name string, links []string) (string, string) {
	for _, l := range links {
		if strings.HasPrefix(name, l+".") {
			return l, name[len(l)+1:]
		}
	}
	return "", name
}

// missingHint guesses why a profiled function isn't in the build.
func missingHint(name string, edges []Edge, compiled map[string]*fnSites, links []string, mainImportPath string, src Source, startOf func(string) (int, bool), profiled func(string) bool) string {
	pkg, rest := splitLink(name, links)
	if mainImportPath != "" && pkg == mainImportPath && compiled["main."+rest] != nil {
		return "the profile names package main by its import path, as a go test binary does, but the program's build calls it main." + rest + "; collect the profile from the real binary"
	}
	// Value and pointer receivers: pkg.T.M and pkg.(*T).M.
	if i := strings.Index(rest, "."); i > 0 && !closureRE.MatchString(rest) {
		recv, method := rest[:i], rest[i+1:]
		var other string
		if strings.HasPrefix(recv, "(*") && strings.HasSuffix(recv, ")") {
			other = recv[2:len(recv)-1] + "." + method
		} else if !strings.HasPrefix(recv, "(") {
			other = "(*" + recv + ")." + method
		}
		if other != "" && compiled[pkg+"."+other] != nil {
			return "the method's receiver changed between value and pointer; it is now " + pkg + "." + other
		}
	}
	if compiled[name+"[...]"] != nil {
		return "now a generic function, " + name + "[...]"
	}
	if strings.HasSuffix(name, "[...]") && compiled[strings.TrimSuffix(name, "[...]")] != nil {
		return "no longer generic: " + strings.TrimSuffix(name, "[...]")
	}
	var moved []string
	for c := range compiled {
		cp, crest := splitLink(c, links)
		if crest == rest && cp != pkg && cp != "" {
			moved = append(moved, c)
		}
	}
	if len(moved) > 0 {
		sort.Strings(moved)
		return "moved to another package: " + strings.Join(moved, ", ")
	}
	if !closureRE.MatchString(rest) {
		var same []string
		for c := range compiled {
			cp, crest := splitLink(c, links)
			if cp != pkg || crest == rest || baseName(c) != baseName(name) || profiled(c) {
				continue
			}
			r1, r2 := receiverOf(rest), receiverOf(crest)
			if (r1 == "") != (r2 == "") || strings.EqualFold(r1, r2) {
				same = append(same, c)
			}
		}
		if len(same) == 1 {
			return "probably now " + same[0] + ", with the same name and a different receiver"
		}
	}
	if closureRE.MatchString(rest) {
		return "a closure; the compiler numbers closures by their order in the function, so adding or removing one before it renames it"
	}
	if strings.HasPrefix(rest, "Test") || strings.HasPrefix(rest, "Benchmark") || strings.HasPrefix(rest, "Fuzz") || strings.HasPrefix(rest, "Example") {
		if mainImportPath != "" && pkg == mainImportPath {
			return "a test function: the profile came from a go test binary, which this build doesn't include"
		}
	}
	if r := renamedTo(pkg, edges, compiled, links, src, startOf, profiled); r != "" {
		return "probably renamed to " + r + ", which makes the same calls at the same lines"
	}
	return "renamed, removed, or has no calls left"
}

// receiverOf returns the receiver type of a method name without its
// package, such as "Book" for "(*Book).Apply", or "" for a function.
func receiverOf(rest string) string {
	i := strings.LastIndex(rest, ".")
	if i < 0 {
		return ""
	}
	r := strings.TrimSuffix(strings.TrimPrefix(rest[:i], "(*"), ")")
	return strings.TrimSuffix(r, "[...]")
}

// renamedTo looks for a function in the same package that the profile
// doesn't name and that makes the missing function's calls at the same
// offsets.
func renamedTo(pkg string, edges []Edge, compiled map[string]*fnSites, links []string, src Source, startOf func(string) (int, bool), profiled func(string) bool) string {
	want := 0
	for _, e := range edges {
		if calleeChecked(e.Callee) {
			want++
		}
	}
	if want == 0 || src == nil {
		return ""
	}
	best, bestScore, tie := "", 0, false
	var names []string
	for c := range compiled {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if cp, _ := splitLink(c, links); cp != pkg || profiled(c) {
			continue
		}
		start, ok := startOf(c)
		if !ok {
			continue
		}
		fs := compiled[c]
		score := 0
		for _, e := range edges {
			if !calleeChecked(e.Callee) {
				continue
			}
			got, _ := namesAt(src, fs, start+e.Offset)
			if contains(got, baseName(e.Callee)) {
				score++
			}
		}
		switch {
		case score > bestScore:
			best, bestScore, tie = c, score, false
		case score == bestScore && score > 0:
			tie = true
		}
	}
	if tie || bestScore < 2 || bestScore*3 < want*2 {
		return ""
	}
	return best
}

// fileSource reads source files from disk.
type fileSource struct {
	dir   string
	lines map[string][]string
	funcs map[string]map[string]funcInfo // file -> name -> info
	lits  map[string]map[int]bool        // file -> lines where a func starts
}

type funcInfo struct {
	start    int
	deferred []string
}

func newFileSource(dir string) *fileSource {
	return &fileSource{dir: dir, lines: map[string][]string{}, funcs: map[string]map[string]funcInfo{}, lits: map[string]map[int]bool{}}
}

func (s *fileSource) path(file string) string {
	if filepath.IsAbs(file) || s.dir == "" {
		return file
	}
	return filepath.Join(s.dir, file)
}

func (s *fileSource) CallName(file string, line, col int) (string, bool) {
	ls, ok := s.lines[file]
	if !ok {
		b, err := os.ReadFile(s.path(file))
		if err == nil {
			ls = strings.Split(string(b), "\n")
		}
		s.lines[file] = ls
	}
	if line < 1 || line > len(ls) {
		return "", false
	}
	text := ls[line-1]
	if col < 2 || col > len(text) || text[col-1] != '(' {
		return "", false
	}
	return identBefore(text[:col-1])
}

// identBefore returns the identifier at the end of text, skipping type
// arguments such as Map[int].
func identBefore(text string) (string, bool) {
	i := len(text)
	if i > 0 && text[i-1] == ']' {
		depth := 0
		for i > 0 {
			i--
			if text[i] == ']' {
				depth++
			} else if text[i] == '[' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if depth != 0 {
			return "", false
		}
	}
	end := i
	for i > 0 {
		c := text[i-1]
		if c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80 {
			i--
			continue
		}
		break
	}
	if i == end {
		return "", false
	}
	return text[i:end], true
}

func (s *fileSource) info(file, pkgLink, name string) (funcInfo, bool) {
	m, ok := s.funcs[file]
	if !ok {
		m = map[string]funcInfo{}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, s.path(file), nil, parser.SkipObjectResolution)
		if err == nil {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil || fd.Name.Name == "init" || fd.Name.Name == "_" {
					continue
				}
				fi := funcInfo{start: fset.Position(fd.Pos()).Line}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if ds, ok := n.(*ast.DeferStmt); ok {
						switch fn := ds.Call.Fun.(type) {
						case *ast.Ident:
							fi.deferred = append(fi.deferred, fn.Name)
						case *ast.SelectorExpr:
							fi.deferred = append(fi.deferred, fn.Sel.Name)
						}
					}
					return true
				})
				m[funcLinkName(pkgLink, fd)] = fi
			}
		}
		s.funcs[file] = m
	}
	fi, ok := m[name]
	return fi, ok
}

func (s *fileSource) FuncStart(file, pkgLink, name string) (int, bool) {
	fi, ok := s.info(file, pkgLink, name)
	return fi.start, ok
}

func (s *fileSource) Deferred(file, pkgLink, name string) []string {
	fi, _ := s.info(file, pkgLink, name)
	return fi.deferred
}

func (s *fileSource) FuncLines(file string) map[int]bool {
	if m, ok := s.lits[file]; ok {
		return m
	}
	var m map[int]bool
	fset := token.NewFileSet()
	if f, err := parser.ParseFile(fset, s.path(file), nil, parser.SkipObjectResolution); err == nil {
		m = map[int]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncDecl:
				m[fset.Position(x.Pos()).Line] = true
			case *ast.FuncLit:
				m[fset.Position(x.Pos()).Line] = true
			case *ast.RangeStmt:
				// The body of a range-over-func loop is compiled as a
				// function (F-range1) that starts at the for keyword.
				m[fset.Position(x.Pos()).Line] = true
			}
			return true
		})
	}
	s.lits[file] = m
	return m
}

// funcLinkName builds the normalized symbol name of a declared function.
func funcLinkName(pkgLink string, fd *ast.FuncDecl) string {
	name := fd.Name.Name
	if fd.Type.TypeParams != nil && len(fd.Type.TypeParams.List) > 0 {
		name += "[...]"
	}
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return pkgLink + "." + name
	}
	t := fd.Recv.List[0].Type
	ptr := false
	if p, ok := t.(*ast.ParenExpr); ok {
		t = p.X
	}
	if st, ok := t.(*ast.StarExpr); ok {
		ptr, t = true, st.X
	}
	recv := ""
	switch x := t.(type) {
	case *ast.Ident:
		recv = x.Name
	case *ast.IndexExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			recv = id.Name + "[...]"
		}
	case *ast.IndexListExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			recv = id.Name + "[...]"
		}
	}
	if ptr {
		recv = "(*" + recv + ")"
	}
	return pkgLink + "." + recv + "." + fd.Name.Name
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*1000) / 10
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return append(append([]string{}, s[:n]...), "...")
	}
	return s
}

// ---------------------------------------------------------------------------
// Building

// goCmd runs the go command in dir and returns combined output, which for
// go build holds the compiler's output.
func goCmd(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// goOut runs the go command in dir and returns its standard output. Messages
// such as "go: downloading" go to standard error and are only shown on error.
func goOut(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go %s failed: %v\n%s", strings.Join(args, " "), err, tail(errb.Bytes(), 20))
	}
	return out.Bytes(), nil
}

type listPkg struct {
	ImportPath string
	Name       string
	Dir        string
	Standard   bool
	Module     *struct {
		Path string
		Main bool
	}
	Error *struct{ Err string }
}

func goList(dir string, args ...string) ([]listPkg, error) {
	out, err := goOut(dir, append([]string{"list", "-e", "-json=ImportPath,Name,Dir,Standard,Module,Error"}, args...)...)
	if err != nil {
		return nil, err
	}
	var pkgs []listPkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("unreadable go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

func tail(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = append([]string{"..."}, lines[len(lines)-n:]...)
	}
	return strings.Join(lines, "\n")
}

// checkOptions are the inputs of the check command.
type checkOptions struct {
	Dir     string
	Package string
	Profile string
	Save    string
}

// Check builds the package with the profile and matches the result.
func Check(o checkOptions) (*Report, error) {
	target, err := goList(o.Dir, o.Package)
	if err != nil {
		return nil, err
	}
	if len(target) != 1 {
		return nil, fmt.Errorf("%q matches %d packages; give exactly one, normally the main package the profile is for", o.Package, len(target))
	}
	t := target[0]
	if t.Error != nil {
		return nil, fmt.Errorf("package %s: %s", o.Package, t.Error.Err)
	}
	modPath := ""
	switch {
	case t.Standard && strings.HasPrefix(t.ImportPath, "cmd/"):
		modPath = "cmd" // the Go toolchain's own commands, such as cmd/compile
	case t.Module != nil && t.Module.Path != "std":
		modPath = t.Module.Path
	default:
		return nil, fmt.Errorf("package %s is not in a module; run pgostale from inside the module", t.ImportPath)
	}
	profile := o.Profile
	if profile == "" {
		profile = filepath.Join(t.Dir, "default.pgo")
		if _, err := os.Stat(profile); err != nil {
			return nil, fmt.Errorf("no --profile given and %s has no default.pgo", t.Dir)
		}
	}
	profile, err = filepath.Abs(profile)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		return nil, err
	}
	ep, err := ReadProfile(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", profile, err)
	}

	deps, err := goList(o.Dir, "-deps", t.ImportPath)
	if err != nil {
		return nil, err
	}
	var pkgs []Package
	modules := map[string]bool{}
	for _, d := range deps {
		if d.Error != nil {
			return nil, fmt.Errorf("package %s: %s", d.ImportPath, d.Error.Err)
		}
		switch {
		case modPath == "cmd" && d.Standard && strings.HasPrefix(d.ImportPath, "cmd/"):
			modules["cmd"] = true
		case d.Module != nil && (d.Module.Path == modPath || d.Module.Main):
			modules[d.Module.Path] = true
		default:
			continue
		}
		link := d.ImportPath
		if d.Name == "main" {
			link = "main"
		}
		pkgs = append(pkgs, Package{ImportPath: d.ImportPath, Name: d.Name, LinkPath: link})
	}
	args := []string{"build", "-pgo=" + profile}
	var mods []string
	for m := range modules {
		mods = append(mods, m)
	}
	sort.Strings(mods)
	for _, m := range mods {
		args = append(args, "-gcflags="+m+"/...=-d=pgodebug=3")
	}
	args = append(args, "-o", os.DevNull, t.ImportPath)
	out, err := goCmd(o.Dir, args...)
	if o.Save != "" {
		if werr := os.WriteFile(o.Save, out, 0o644); werr != nil {
			return nil, werr
		}
	}
	if err != nil {
		return nil, fmt.Errorf("go %s failed: %v\n%s", strings.Join(args, " "), err, tail(out, 20))
	}
	sites, err := ParseCallSites(out)
	if err != nil {
		return nil, err
	}
	mainPath := ""
	if t.Name == "main" {
		mainPath = t.ImportPath
	}
	rep, err := Match(ep, sites, pkgs, mainPath, newFileSource(o.Dir))
	if err != nil {
		return nil, err
	}
	if v, err := goOut(o.Dir, "env", "GOVERSION"); err == nil {
		rep.GoVersion = strings.TrimSpace(string(v))
	}
	rep.Package = t.ImportPath
	rep.Profile = o.Profile
	if rep.Profile == "" {
		rep.Profile = profile
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Output

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// WriteText prints the report for people and agents.
func WriteText(w io.Writer, r *Report, limit int) {
	share := func(x int64) string { return fmt.Sprintf("%.1f%%", float64(x)/float64(r.Total)*100) }
	fmt.Fprintf(w, "Profile %s on %s\n", r.Profile, r.Package)
	fmt.Fprintf(w, "Profile: %s, %s, total weight %d.\n", plural(r.Edges, "call edge", "call edges"), plural(r.HotTotal, "hot", "hot"), r.Total)
	fmt.Fprintf(w, "In this module: %s of the weight (%s). Standard library and other modules: %s, not checked.",
		share(r.Module), plural(r.HotEdges, "hot edge", "hot edges"), share(r.Outside))
	if r.Inserted > 0 {
		fmt.Fprintf(w, " Calls to compiler-inserted runtime helpers: %s, skipped.", share(r.Inserted))
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w)
	if r.ScoreBasis == "hot" {
		fmt.Fprintf(w, "Still matches: %.1f%% of this module's hot weight (%d of %d hot edges), %.1f%% of all its weight.\n",
			r.Score, r.HotMatched, r.HotEdges, pct(r.Matched, r.Module))
	} else {
		fmt.Fprintf(w, "Still matches: %.1f%% of this module's weight. None of its edges is hot.\n", r.Score)
	}
	if r.Module < 50 {
		fmt.Fprintf(w, "Only %d of the profile's weight is in this module, so this result rests on little data.\n", r.Module)
	}
	if len(r.Lost) == 0 {
		fmt.Fprintln(w, "Every call edge in this module still matches the code.")
		return
	}
	if r.Below {
		fmt.Fprintf(w, "Below the minimum of %.0f%%: the profile is stale for this code. Collect a new one from the current build.\n", r.Min)
	}
	if len(r.Missing) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Functions in the profile that this build doesn't have:")
		for i, m := range r.Missing {
			if limit > 0 && i == limit {
				fmt.Fprintf(w, "  ... %d more (use --limit 0 or --json)\n", len(r.Missing)-limit)
				break
			}
			hot := ""
			if m.HotEdges > 0 {
				hot = ", " + plural(m.HotEdges, "hot edge", "hot edges")
			}
			fmt.Fprintf(w, "  %s  weight %d (%s)%s\n      %s\n", m.Name, m.Weight, share(m.Weight), hot, m.Hint)
		}
	}
	var moved []LostEdge
	for _, l := range r.Lost {
		if l.Reason != "function-missing" {
			moved = append(moved, l)
		}
	}
	if len(moved) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Call sites that moved or changed in functions that still exist:")
		for i, l := range moved {
			if limit > 0 && i == limit {
				fmt.Fprintf(w, "  ... %d more (use --limit 0 or --json)\n", len(moved)-limit)
				break
			}
			hot := ""
			if l.Hot {
				hot = ", hot"
			}
			fmt.Fprintf(w, "  %s +%d -> %s  weight %d (%s)%s\n", l.Caller, l.Offset, l.Callee, l.Weight, share(l.Weight), hot)
			switch {
			case l.Reason == "callee-changed":
				fmt.Fprintf(w, "      %s calls %s now; the compiler still treats it as this hot call site\n", relPath(l.Where), l.Now)
			case l.Where != "":
				fmt.Fprintf(w, "      no call at %s now\n", relPath(l.Where))
			case closureRE.MatchString(normalizeName(l.Caller)):
				fmt.Fprintln(w, "      no call at this offset now; closures are numbered by position, so this may be a different closure")
			default:
				fmt.Fprintln(w, "      no call at this offset now")
			}
		}
	}
	if len(r.Unresolved) > 0 {
		fmt.Fprintf(w, "\nCould not line up the compiler's call sites for %s; their edges were matched by weight only.\n", strings.Join(r.Unresolved, ", "))
	}
}

// WriteMarkdown prints a bounded report suitable for a pull request comment.
func WriteMarkdown(w io.Writer, r *Report, limit int) {
	fmt.Fprintln(w, "## PGO profile match")
	fmt.Fprintf(w, "\nProfile: %s. Package: %s.\n", markdownCell(r.Profile), markdownCell(r.Package))
	if r.ScoreBasis == "hot" {
		fmt.Fprintf(w, "\nStill matches: **%.1f%%** of this module's hot weight (%d of %d hot edges), %.1f%% of all its weight.\n",
			r.Score, r.HotMatched, r.HotEdges, pct(r.Matched, r.Module))
	} else {
		fmt.Fprintf(w, "\nStill matches: **%.1f%%** of this module's weight. None of its edges is hot.\n", r.Score)
	}
	fmt.Fprintf(w, "\nChecked module weight: %.1f%% of the profile; %.1f%% is outside the module and %.1f%% is compiler-inserted, not checked.\n",
		pct(r.Module, r.Total), pct(r.Outside, r.Total), pct(r.Inserted, r.Total))
	if r.Module < 50 {
		fmt.Fprintf(w, "\nOnly %d of the profile's weight is in this module, so this result rests on little data.\n", r.Module)
	}
	if r.Below {
		fmt.Fprintf(w, "\nBelow the minimum of %.0f%%: collect a new profile from the current build.\n", r.Min)
	}
	fmt.Fprintln(w, "\nThis reports compiler profile matching, not a measured slowdown.")
	fmt.Fprintln(w, "\n### Missing functions\n\n| Function | Weight (% of profile) | Hot edges | Hint |\n| --- | --- | --- | --- |")
	for i, m := range r.Missing {
		if limit > 0 && i == limit {
			break
		}
		fmt.Fprintf(w, "| %s | %d (%.1f%%) | %d | %s |\n", markdownCell(m.Name), m.Weight, pct(m.Weight, r.Total), m.HotEdges, markdownCell(m.Hint))
	}
	markdownRemainder(w, len(r.Missing), limit)
	var moved, changed []LostEdge
	for _, edge := range r.Lost {
		switch edge.Reason {
		case "function-missing":
			// Already listed in the missing-functions table.
		case "callee-changed":
			changed = append(changed, edge)
		default:
			moved = append(moved, edge)
		}
	}
	for _, section := range []struct {
		title string
		edges []LostEdge
	}{{"Moved call sites", moved}, {"Calls that changed", changed}} {
		fmt.Fprintf(w, "\n### %s\n\n| Caller | Offset | Callee | Weight (%% of profile) | Hot | Location | Current call |\n| --- | --- | --- | --- | --- | --- | --- |\n", section.title)
		for i, edge := range section.edges {
			if limit > 0 && i == limit {
				break
			}
			fmt.Fprintf(w, "| %s | +%d | %s | %d (%.1f%%) | %t | %s | %s |\n",
				markdownCell(edge.Caller), edge.Offset, markdownCell(edge.Callee), edge.Weight, pct(edge.Weight, r.Total), edge.Hot, markdownCell(relPath(edge.Where)), markdownCell(edge.Now))
		}
		markdownRemainder(w, len(section.edges), limit)
	}
	if len(changed) > 0 {
		fmt.Fprintln(w, "\nFor changed calls, the compiler still treats the location as this hot call site.")
	}
	if len(r.Unresolved) > 0 {
		fmt.Fprintf(w, "\nCould not line up the compiler's call sites for %s; their edges were matched by weight only.\n", markdownCell(strings.Join(r.Unresolved, ", ")))
	}
}

func markdownRemainder(w io.Writer, total, limit int) {
	if total == 0 {
		fmt.Fprintln(w, "\nNone.")
	} else if limit > 0 && total > limit {
		fmt.Fprintf(w, "\n%d more (use --limit 0 or --json).\n", total-limit)
	}
}

// Escape data before placing it in a Markdown paragraph or table cell.
func markdownCell(s string) string {
	return strings.NewReplacer("\\", "\\\\", "|", "\\|", "`", "\\`", "*", "\\*", "_", "\\_", "[", "\\[", "]", "\\]", "~", "\\~",
		"\r\n", "<br>", "\r", "<br>", "\n", "<br>").Replace(html.EscapeString(s))
}

// relPath shortens an absolute path under the working directory.
func relPath(p string) string {
	wd, err := os.Getwd()
	if err != nil || !filepath.IsAbs(p) {
		return p
	}
	if r, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

// WriteEdges prints the heaviest edges of a profile.
func WriteEdges(w io.Writer, ep *EdgeProfile, top int) {
	writeEdges(w, ep, top, "")
}

func writeEdges(w io.Writer, ep *EdgeProfile, top int, caller string) {
	hot := len(ep.HotEdges())
	fmt.Fprintf(w, "%s, %s, total weight %d\n", plural(len(ep.Edges), "call edge", "call edges"), plural(hot, "hot", "hot"), ep.Total)
	matches := len(ep.Edges)
	if caller != "" {
		matches = 0
		for _, edge := range ep.Edges {
			if strings.Contains(edge.Caller, caller) {
				matches++
			}
		}
		fmt.Fprintf(w, "Caller contains %q: %s\n", caller, plural(matches, "matching call edge", "matching call edges"))
	}
	shown := 0
	for i, e := range ep.Edges {
		if !strings.Contains(e.Caller, caller) {
			continue
		}
		if top > 0 && shown == top {
			fmt.Fprintf(w, "... %d more\n", matches-shown)
			break
		}
		shown++
		mark := " "
		if i < hot {
			mark = "*"
		}
		fmt.Fprintf(w, "%s %6.2f%%  %s +%d -> %s\n", mark, float64(e.Weight)/float64(ep.Total)*100, e.Caller, e.Offset, e.Callee)
	}
}

// filterCaller preserves profile ordering and leaves the profile metadata intact.
func filterCaller(edges []Edge, caller string) []Edge {
	if caller == "" {
		return edges
	}
	filtered := make([]Edge, 0)
	for _, edge := range edges {
		if strings.Contains(edge.Caller, caller) {
			filtered = append(filtered, edge)
		}
	}
	return filtered
}

// ---------------------------------------------------------------------------
// Command line

const usage = `pgostale checks whether a Go PGO profile still matches your code.

Usage:
  pgostale check [--profile FILE] [--min PCT] [--fail] [--json | --markdown] [--limit N] [--save FILE] [-C DIR] [PACKAGE]
  pgostale edges [--top N] [--caller PATTERN] [--json] PROFILE
  pgostale version

check builds PACKAGE (default ".") with the profile, the way go build -pgo
does, and reports how much of the profile's hot call weight in your module
the compiler still matches. The profile defaults to default.pgo in the
package directory. edges lists a profile's heaviest call edges.

Exit codes: 0 report printed, 1 --fail and the match is below --min,
2 bad usage or input that can't be checked.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "edges":
		return runEdges(args[1:], stdout, stderr)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "pgostale", version)
		return 0
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "pgostale: unknown command %q\n\n%s", args[0], usage)
	return 2
}

// parseInterspersed lets flags come after positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func runCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o checkOptions
	fs.StringVar(&o.Profile, "profile", "", "profile `file` (default: default.pgo in the package directory)")
	fs.StringVar(&o.Dir, "C", "", "run in `dir`")
	fs.StringVar(&o.Save, "save", "", "save the build output with the compiler's call site data to `file`")
	minPct := fs.Float64("min", 90, "minimum share of hot weight that must still match, in `percent`")
	fail := fs.Bool("fail", false, "exit 1 when the match is below --min")
	asJSON := fs.Bool("json", false, "print the full report as JSON")
	asMarkdown := fs.Bool("markdown", false, "print the report as Markdown tables")
	limit := fs.Int("limit", 15, "list at most `n` entries per section, 0 for all")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if *asJSON && *asMarkdown {
		fmt.Fprintln(stderr, "pgostale check: --markdown and --json cannot be combined")
		return 2
	}
	if len(pos) > 1 {
		fmt.Fprintln(stderr, "pgostale check: give one package")
		return 2
	}
	if *minPct < 0 || *minPct > 100 {
		fmt.Fprintln(stderr, "pgostale check: --min must be between 0 and 100")
		return 2
	}
	o.Package = "."
	if len(pos) == 1 {
		o.Package = pos[0]
	}
	rep, err := Check(o)
	if err != nil {
		fmt.Fprintf(stderr, "pgostale: can't check: %v\n", err)
		return 2
	}
	rep.Min = *minPct
	rep.Below = rep.Score < *minPct
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else if *asMarkdown {
		WriteMarkdown(stdout, rep, *limit)
	} else {
		WriteText(stdout, rep, *limit)
	}
	if *fail && rep.Below {
		return 1
	}
	return 0
}

func runEdges(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("edges", flag.ContinueOnError)
	fs.SetOutput(stderr)
	top := fs.Int("top", 20, "show the `n` heaviest edges, 0 for all")
	caller := fs.String("caller", "", "only show callers containing `pattern` (case-sensitive substring)")
	asJSON := fs.Bool("json", false, "print all edges as JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "pgostale edges: give one profile file")
		return 2
	}
	data, err := os.ReadFile(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "pgostale: %v\n", err)
		return 2
	}
	ep, err := ReadProfile(data)
	if err != nil {
		fmt.Fprintf(stderr, "pgostale: can't read %s: %v\n", pos[0], err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			Format string `json:"format"`
			Total  int64  `json:"totalWeight"`
			Hot    int    `json:"hotEdges"`
			Edges  []Edge `json:"edges"`
		}{ep.Format, ep.Total, len(ep.HotEdges()), filterCaller(ep.Edges, *caller)})
		return 0
	}
	writeEdges(stdout, ep, *top, *caller)
	return 0
}
