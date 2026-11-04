// Package parse reads synthetic transaction lines.
package parse

import (
	"errors"
	"strconv"
	"strings"
)

// Txn is one transfer between two accounts, in cents.
type Txn struct {
	From, To string
	Cents    int64
}

var errFields = errors.New("parse: want 3 fields")

// ParseLine reads "from,to,cents". Amounts are whole cents; fields may
// have spaces around them.
//
// It returns errFields when a line has the wrong number of fields.
func ParseLine(s string) (Txn, error) {
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
