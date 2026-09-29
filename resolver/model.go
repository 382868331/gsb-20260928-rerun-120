package resolver

import (
	"fmt"
	"strings"
)

// Kind identifies a record kind known to the resolver.
type Kind string

const (
	A     Kind = "A"
	CNAME Kind = "CNAME"
	NS    Kind = "NS"
)

// Record is a single resource record inside a fictional zone.
// Data holds the record payload: an address, an alias target or a
// nameserver name depending on Kind.
type Record struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	Data string `json:"data"`
	TTL  int    `json:"ttl"`
}

// NormalizeName lower-cases a name, trims whitespace and removes a
// trailing dot so names compare consistently.
func NormalizeName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".")
	return n
}

// HasSuffixName reports whether name ends with parent as a label boundary:
// host.a.example is under example.example and example itself both count.
func HasSuffixName(name, parent string) bool {
	if parent == "" {
		return true
	}
	if name == parent {
		return true
	}
	return strings.HasSuffix(name, "."+parent)
}

func validateRecord(r Record) error {
	if NormalizeName(r.Name) == "" {
		return fmt.Errorf("record has empty name")
	}
	switch r.Kind {
	case A, CNAME, NS:
		if strings.TrimSpace(r.Data) == "" {
			return fmt.Errorf("%s record %q has empty data", r.Kind, r.Name)
		}
		if r.TTL < 0 {
			return fmt.Errorf("record %q has negative ttl", r.Name)
		}
	default:
		return fmt.Errorf("record %q has unsupported kind %q", r.Name, r.Kind)
	}
	return nil
}
