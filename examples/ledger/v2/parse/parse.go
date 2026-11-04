// Package parse reads synthetic transaction lines.
package parse

import (
	"errors"
	"hash/fnv"
	"strconv"
	"strings"
)

// Txn is one transfer between two accounts, in cents.
type Txn struct {
	From, To string
	Cents    int64
}

var errFields = errors.New("parse: want 3 fields")

// Line reads "from,to,cents".
func Line(s string) (Txn, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 3 {
		return Txn{}, errFields
	}
	n, err := strconv.ParseInt(strings.TrimSpace(parts[2]), 10, 64)
	if err != nil {
		return Txn{}, err
	}
	return Txn{From: normalize(parts[0]), To: normalize(parts[1]), Cents: n}, nil
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// Digest hashes all lines, as a stand-in for an audit checksum.
func Digest(lines []string) uint64 {
	h := fnv.New64a()
	for _, l := range lines {
		h.Write([]byte(l))
	}
	return h.Sum64()
}
