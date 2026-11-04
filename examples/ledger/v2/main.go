// Command ledger is a made-up service for the pgostale examples. It
// generates synthetic transactions, parses them and applies them to a book
// of accounts. All names and amounts are generated.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime/pprof"
	"strconv"
	"time"

	"example.com/ledger/book"
	"example.com/ledger/parse"
)

func main() {
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile to `file`")
	seconds := flag.Float64("seconds", 3, "run the workload for this many seconds")
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	lines := Generate(20000)
	end := time.Now().Add(time.Duration(*seconds * float64(time.Second)))
	rounds := 0
	var sum uint64
	for time.Now().Before(end) {
		b := Run(lines)
		sum += parse.Digest(lines)
		rounds++
		if rounds == 1 {
			fmt.Fprintf(os.Stderr, "accounts=%d top=%s\n", b.Len(), b.Top(1)[0])
		}
	}
	fmt.Fprintf(os.Stderr, "rounds=%d digest=%d\n", rounds, sum%1000)
}

// Generate makes n synthetic transaction lines like "acct-17,acct-4,1250".
func Generate(n int) []string {
	lines := make([]string, n)
	x := uint32(1)
	for i := range lines {
		x = x*1664525 + 1013904223
		from := x % 97
		x = x*1664525 + 1013904223
		to := x % 89
		x = x*1664525 + 1013904223
		lines[i] = "acct-" + strconv.Itoa(int(from)) + ",acct-" + strconv.Itoa(int(to)) + "," + strconv.Itoa(int(x%100000))
	}
	return lines
}

// Run parses every line and applies it to a new book.
func Run(lines []string) *book.Book {
	b := book.New(book.FeeRule{Basis: 25}, book.LimitRule{Max: 90000})
	var txns []parse.Txn
	skipped := 0
	skip := func(l string) bool {
		if len(l) == 0 {
			skipped++
			return true
		}
		return false
	}
	apply := func(l string) {
		if skip(l) {
			return
		}
		t, err := parse.Line(l)
		if err != nil {
			return
		}
		b.Apply(t)
		txns = append(txns, t)
	}
	for _, l := range lines {
		apply(l)
	}
	fees := book.Map(txns, func(t parse.Txn) int64 { return t.Cents * 25 / 10000 })
	b.Fees = book.Total(fees)
	return b
}
