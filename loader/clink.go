package loader

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// linkTokenRe matches a single valid link flag: -l<name> or -L<path>.
// Everything after -l or -L must be one or more non-whitespace characters.
var linkTokenRe = regexp.MustCompile(`^-[lL]\S+$`)

// linkDirectivePrefix is the marker that introduces link flags in C source
// files. It must appear in a `//` line comment at the top of the file.
const linkDirectivePrefix = "spython-link:"

// maxLinkScanLines bounds how far into the file we scan for directives. This
// keeps the loader from reading large C files in full and makes the convention
// "put directives at the top" enforceable.
const maxLinkScanLines = 20

// ParseLinkDirectives scans the first maxLinkScanLines lines of a C source
// file for `// spython-link: <flag>...` comments. Each matching line
// contributes its whitespace-split tokens, which must be -l<name> or -L<path>.
// Any other token (or a malformed directive) is a hard error. Returns the
// flags in order of appearance; de-duplication is the caller's job.
func ParseLinkDirectives(cFilePath string) ([]string, error) {
	f, err := os.Open(cFilePath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cFilePath, err)
	}
	defer f.Close()

	var flags []string
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() && line < maxLinkScanLines {
		line++
		text := strings.TrimLeft(scanner.Text(), " \t")
		if !strings.HasPrefix(text, "//") {
			continue
		}
		rest := strings.TrimLeft(strings.TrimPrefix(text, "//"), " \t")
		if !strings.HasPrefix(rest, linkDirectivePrefix) {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(rest, linkDirectivePrefix))
		if body == "" {
			return nil, fmt.Errorf("%s:%d: empty spython-link directive", cFilePath, line)
		}
		for _, tok := range strings.Fields(body) {
			if !linkTokenRe.MatchString(tok) {
				return nil, fmt.Errorf("%s:%d: invalid link token %q (only -l<name> and -L<path> are allowed)",
					cFilePath, line, tok)
			}
			flags = append(flags, tok)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", cFilePath, err)
	}
	return flags, nil
}

// DedupeFlags returns flags with duplicates removed, preserving first-seen
// order.
func DedupeFlags(flags []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(flags))
	for _, f := range flags {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}
