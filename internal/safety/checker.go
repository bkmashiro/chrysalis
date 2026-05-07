// Package safety implements a fast static pre-check for user-submitted Python code.
// It is a lightweight heuristic, not a security boundary — WASM SFI is the actual boundary.
package safety

import (
	"fmt"
	"regexp"
	"strings"
)

// Result is the output of the safety check.
type Result struct {
	Safe    bool
	Blocked []string
}

// blockedModules is the set of Python standard-library modules we disallow.
var blockedModules = []string{
	"os", "sys", "socket", "subprocess", "ctypes",
	"importlib", "pickle", "marshal", "shutil", "pathlib",
	"threading", "multiprocessing", "asyncio", "signal",
	"resource", "pty", "io", "tempfile", "builtins",
	"gc", "weakref", "inspect", "types", "traceback",
}

// blockedBuiltins are Python built-in call patterns we disallow.
var blockedBuiltins = []string{
	"eval(", "exec(", "compile(", "__import__(", "open(",
	"breakpoint(", "globals(", "locals(", "vars(",
	"getattr(", "setattr(", "delattr(",
}

// importRe matches: import X, import X as Y, from X import ..., from X.Y import ...
var importRe = regexp.MustCompile(`(?m)(?:^|\s)(?:import|from)\s+([\w.]+)`)

// Check returns a Result indicating whether the code appears safe to run.
func Check(code string) Result {
	result := Result{Safe: true}

	// Check imports.
	for _, match := range importRe.FindAllStringSubmatch(code, -1) {
		if len(match) < 2 {
			continue
		}
		modName := strings.SplitN(match[1], ".", 2)[0]
		for _, blocked := range blockedModules {
			if modName == blocked {
				result.Safe = false
				result.Blocked = append(result.Blocked,
					fmt.Sprintf("import of blocked module %q", modName))
				break
			}
		}
	}

	// Check blocked built-in calls.
	for _, b := range blockedBuiltins {
		if strings.Contains(code, b) {
			result.Safe = false
			result.Blocked = append(result.Blocked,
				fmt.Sprintf("call to blocked builtin %q", strings.TrimSuffix(b, "(")))
		}
	}

	return result
}
