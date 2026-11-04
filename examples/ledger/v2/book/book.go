// Package book keeps account balances.
package book

import (
	"fmt"
	"sort"

	"example.com/ledger/parse"
)

// Rule adjusts a transfer before it is applied.
type Rule interface {
	Adjust(cents int64) int64
}

// FeeRule takes a fee in basis points.
type FeeRule struct{ Basis int64 }

// Adjust implements Rule.
func (f FeeRule) Adjust(cents int64) int64 { return cents - cents*f.Basis/10000 }

// LimitRule caps a transfer.
type LimitRule struct{ Max int64 }

// Adjust implements Rule.
func (l LimitRule) Adjust(cents int64) int64 {
	if cents > l.Max {
		return l.Max
	}
	return cents
}

// Account is one balance with its history length.
type Account struct {
	Name    string
	cents   int64
	entries int
}

// Balance returns the balance in cents.
func (a Account) Balance() int64 { return a.cents }

// Book is a set of accounts.
type Book struct {
	accounts map[string]*Account
	rules    []Rule
	Fees     int64
}

// New returns an empty book that applies rules in order.
func New(rules ...Rule) *Book {
	return &Book{accounts: map[string]*Account{}, rules: rules}
}

// Apply moves money between two accounts.
func (b *Book) Apply(t parse.Txn) {
	if t.From == t.To {
		return
	}
	cents := t.Cents
	for _, r := range b.rules {
		cents = r.Adjust(cents)
	}
	from := b.account(t.From)
	to := b.account(t.To)
	from.cents -= cents
	to.cents += cents
	from.entries++
	to.entries++
}

func (b *Book) account(name string) *Account {
	a := b.accounts[name]
	if a == nil {
		a = &Account{Name: name}
		b.accounts[name] = a
	}
	return a
}

// Len returns the number of accounts.
func (b *Book) Len() int { return len(b.accounts) }

// Top returns the n accounts with the highest balance.
func (b *Book) Top(n int) []string {
	all := make([]*Account, 0, len(b.accounts))
	for _, a := range b.accounts {
		all = append(all, a)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Balance() != all[j].Balance() {
			return all[i].Balance() > all[j].Balance()
		}
		return all[i].Name < all[j].Name
	})
	var out []string
	for _, a := range all[:n] {
		out = append(out, fmt.Sprintf("%s=%d", a.Name, a.Balance()))
	}
	return out
}

// Total sums any numeric values.
func Total[T int64 | float64](xs []T) T {
	var s T
	for _, x := range xs {
		s += x
	}
	return s
}

// Map applies f to each element.
func Map[T, U any](xs []T, f func(T) U) []U {
	out := make([]U, len(xs))
	for i, x := range xs {
		out[i] = f(x)
	}
	return out
}
