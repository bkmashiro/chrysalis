// Package filter implements YAML-based function allow/deny lists for the bridge dispatcher.
package filter

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FilterConfig is the YAML-deserialized form of a filter profile.
type FilterConfig struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Default     string   `yaml:"default"` // "allow" or "deny"
	Allow       []string `yaml:"allow"`
	Deny        []string `yaml:"deny"`
}

// Filter holds compiled allow/deny rules for a profile.
type Filter struct {
	// exact maps fully-qualified function names to their allow (true) / deny (false) status.
	exact map[string]bool
	// allowPatterns holds wildcard patterns that allow a function.
	allowPatterns []string
	// denyPatterns holds wildcard patterns that deny a function.
	denyPatterns []string
	// defaultAllow is the fallback when no rule matches.
	defaultAllow bool
	// ProfileName is the name of the loaded profile.
	ProfileName string
}

// Allow returns true if fnName is permitted by this filter.
//
// Evaluation order (deny-wins at same specificity):
//  1. Exact deny  → false
//  2. Exact allow → true
//  3. Wildcard deny → false
//  4. Wildcard allow → true
//  5. Default
func (f *Filter) Allow(fnName string) bool {
	// 1. Exact deny
	if allowed, ok := f.exact[fnName]; ok && !allowed {
		return false
	}
	// 2. Exact allow
	if allowed, ok := f.exact[fnName]; ok && allowed {
		return true
	}
	// 3. Wildcard deny
	for _, pat := range f.denyPatterns {
		if matchWildcard(pat, fnName) {
			return false
		}
	}
	// 4. Wildcard allow
	for _, pat := range f.allowPatterns {
		if matchWildcard(pat, fnName) {
			return true
		}
	}
	// 5. Default
	return f.defaultAllow
}

// matchWildcard performs glob-style matching where '*' matches any sequence of characters
// that does not cross a '.' boundary, and '**' matches across '.'.
// For simplicity we use a flat approach: '*' matches anything including '.'.
func matchWildcard(pattern, name string) bool {
	// Split on '*' and check containment in order.
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.SplitN(pattern, "*", 2)
	prefix, suffix := parts[0], parts[1]
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	rest := name[len(prefix):]
	if suffix == "" {
		return true
	}
	// suffix may itself contain '*'
	return matchWildcard(suffix, rest) || strings.HasSuffix(rest, suffix) && !strings.Contains(suffix, "*")
}

// LoadFilter reads a YAML profile from filterDir and returns a compiled Filter.
func LoadFilter(filterDir, profileName string) (*Filter, error) {
	path := filepath.Join(filterDir, profileName+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("filter profile %q not found: %w", profileName, err)
	}

	var cfg FilterConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid filter profile %q: %w", profileName, err)
	}

	f := &Filter{
		exact:        make(map[string]bool),
		defaultAllow: strings.ToLower(cfg.Default) == "allow",
		ProfileName:  profileName,
	}

	for _, pat := range cfg.Allow {
		if strings.Contains(pat, "*") {
			f.allowPatterns = append(f.allowPatterns, pat)
		} else {
			f.exact[pat] = true
		}
	}
	for _, pat := range cfg.Deny {
		if strings.Contains(pat, "*") {
			f.denyPatterns = append(f.denyPatterns, pat)
		} else {
			f.exact[pat] = false
		}
	}

	return f, nil
}

// AllowAll returns a filter that permits every function.
func AllowAll() *Filter {
	return &Filter{
		exact:        make(map[string]bool),
		defaultAllow: true,
		ProfileName:  "allow-all",
	}
}
