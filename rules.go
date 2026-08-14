package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/opentalon/tln-language/pkg/tln"
)

// ruleStore is a filesystem-backed store: one rule = one `<name>.tln`
// file in RootDir. Rule names are validated against ruleNamePattern so
// they can't escape RootDir via "../" or land on awkward filenames.
type ruleStore struct {
	RootDir string
}

// ruleNamePattern allows only safe filesystem-friendly characters and
// caps length. Sufficient to disallow path traversal, leading dots,
// and shell-special chars.
var ruleNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_\-]{0,63}$`)

// errInvalidRuleName is returned when a name fails ruleNamePattern.
// Exported as a sentinel so the HTTP handler can return 400 cleanly.
var errInvalidRuleName = errors.New("rule name must match [a-zA-Z0-9][a-zA-Z0-9_-]{0,63}")

// errRuleNotFound is returned when Read/Delete can't find a rule. The
// HTTP handler maps this to 404.
var errRuleNotFound = errors.New("rule not found")

// pathFor turns a validated rule name into its on-disk path.
func (s *ruleStore) pathFor(name string) (string, error) {
	if !ruleNamePattern.MatchString(name) {
		return "", errInvalidRuleName
	}
	return filepath.Join(s.RootDir, name+".tln"), nil
}

// Save writes (creates or overwrites) the rule. Tln source is
// validated via the SDK before the file lands on disk — a syntactically
// invalid rule never gets stored. Detect-bearing rules are accepted
// (ErrRequiresFactStore from RunWorkflow is the SDK's "this is a
// detect rule, not a workflow" signal; both are valid Tln programs).
func (s *ruleStore) Save(name, src string) error {
	path, err := s.pathFor(name)
	if err != nil {
		return err
	}
	if err := validateRuleSource(src); err != nil {
		return err
	}
	if err := os.MkdirAll(s.RootDir, 0o755); err != nil {
		return fmt.Errorf("create rules dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil { //nolint:gosec // rules are owner-readable by design
		return fmt.Errorf("write rule: %w", err)
	}
	return nil
}

// Read returns the Tln source for the named rule.
func (s *ruleStore) Read(name string) (string, error) {
	path, err := s.pathFor(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errRuleNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read rule: %w", err)
	}
	return string(b), nil
}

// Delete removes the named rule. Idempotent — deleting a missing
// rule returns errRuleNotFound so callers can distinguish "I did
// nothing" from "I deleted it".
func (s *ruleStore) Delete(name string) error {
	path, err := s.pathFor(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errRuleNotFound
		}
		return fmt.Errorf("delete rule: %w", err)
	}
	return nil
}

// List returns the names of all rules currently on disk, sorted
// lexically for stable output.
func (s *ruleStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.RootDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".tln") {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".tln"))
	}
	sort.Strings(names)
	return names, nil
}

// validateRuleSource confirms src is parseable Tln. Uses RunWorkflow
// as a dry compile — for detect-bearing rules the SDK returns
// ErrRequiresFactStore after parse+plan, which means "valid Tln, just
// not a workflow"; we accept that. Compile errors propagate.
func validateRuleSource(src string) error {
	_, err := tln.RunWorkflow(context.Background(), src)
	if err == nil {
		return nil
	}
	if errors.Is(err, tln.ErrRequiresFactStore) {
		return nil
	}
	return err
}
