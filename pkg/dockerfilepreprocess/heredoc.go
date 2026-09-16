package dockerfilepreprocess

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
)

var (
	runCatRedirectHeredocPattern = regexp.MustCompile(`^([ \t]*)RUN[ \t]+cat[ \t]+>[ \t]+([^ \t]+)[ \t]+<<(-?)[ \t]*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)(['\"]?)[ \t]*$`)
	runCatHeredocRedirectPattern = regexp.MustCompile(`^([ \t]*)RUN[ \t]+cat[ \t]+<<(-?)[ \t]*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)(['\"]?)[ \t]+>[ \t]+([^ \t]+)[ \t]*$`)
	runCatAppendHeredocPattern   = regexp.MustCompile(`^[ \t]*RUN[ \t]+cat[ \t]+>>[ \t]+[^ \t]+[ \t]+<<`)
)

// PreprocessDockerfile rewrites legacy shell heredocs that Dockerfile parsing
// cannot handle into BuildKit Dockerfile heredocs.
func PreprocessDockerfile(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	processed, changed, err := TransformLegacyHeredocs(raw)
	if err != nil || !changed {
		return changed, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(path, processed, info.Mode()); err != nil {
		return false, err
	}
	return true, nil
}

func TransformLegacyHeredocs(raw []byte) ([]byte, bool, error) {
	lines := splitDockerfileLines(string(raw))
	out := make([]string, 0, len(lines))
	changed := false
	escape := byte('\\')
	directives := &parser.DirectiveParser{}

	for idx := 0; idx < len(lines); idx++ {
		start := idx
		directive, err := directives.ParseLine([]byte(strings.TrimSpace(lines[idx])))
		if err != nil {
			return nil, false, fmt.Errorf("Dockerfile line %d: %w", start+1, err)
		}
		if directive != nil && directive.Name == "escape" && (directive.Value == "\\" || directive.Value == "`") {
			escape = directive.Value[0]
		}
		plain, headerEnd := instructionHeader(lines, start, escape)
		idx = headerEnd
		instruction, command := splitInstruction(plain)
		onbuild := instruction == "ONBUILD"
		if onbuild {
			instruction, command = splitInstruction(command)
		}
		if instruction != "RUN" && instruction != "COPY" && instruction != "ADD" {
			out = append(out, lines[start:idx+1]...)
			continue
		}
		// Only the keyword is normalized; untouched source is emitted verbatim.
		indent := plain[:len(plain)-len(strings.TrimLeft(plain, " \t"))]
		plain = indent + instruction + " " + strings.TrimSpace(command)

		if runCatAppendHeredocPattern.MatchString(plain) {
			return nil, false, fmt.Errorf("line %d: append heredoc with cat >> is not supported; use Dockerfile COPY heredoc or split the append into an explicit RUN", start+1)
		}

		spec, ok, err := parseLegacyCatHeredoc(plain, start+1)
		if err != nil {
			return nil, false, err
		}
		if ok && (onbuild || strings.ContainsAny(spec.target, "$`\\\"'~*?[]{};&|<>()")) {
			ok = false // COPY cannot preserve shell expansion or command syntax in the target.
		}
		var docs []parser.Heredoc
		if instruction == "RUN" {
			docs, err = instructionHeredocs(command)
		} else {
			docs, err = dockerfileHeredocs(command)
		}
		if err != nil {
			return nil, false, fmt.Errorf("Dockerfile line %d: %w", start+1, err)
		}
		if instruction == "RUN" && len(docs) > 0 && !ok && !nativeRunHeredoc(command) {
			return nil, false, fmt.Errorf("Dockerfile line %d: unsupported RUN heredoc %q; shell command chains, append redirects and other complex forms cannot be safely rewritten; split file creation into a standalone RUN cat > /path <<'EOF', or use native COPY <<'EOF' /path with separate RUN instructions", start+1, docs[0].Name)
		}
		for _, doc := range docs {
			end := findHeredocEnd(lines, idx+1, doc.Name, doc.Chomp)
			if end < 0 {
				return nil, false, fmt.Errorf("Dockerfile line %d: heredoc delimiter %q was not found", start+1, doc.Name)
			}
			idx = end
		}

		if ok {
			out = append(out, fmt.Sprintf("%sCOPY <<%s %s\n", spec.indent, spec.delimiterToken, spec.target))
			out = append(out, lines[headerEnd+1:idx+1]...)
			changed = true
		} else {
			out = append(out, lines[start:idx+1]...)
		}
	}

	if !changed {
		return raw, false, nil
	}
	return []byte(strings.Join(out, "")), true, nil
}

type legacyHeredocSpec struct {
	indent         string
	target         string
	delimiterToken string
}

func parseLegacyCatHeredoc(line string, lineNumber int) (legacyHeredocSpec, bool, error) {
	if match := runCatRedirectHeredocPattern.FindStringSubmatch(line); match != nil {
		quoteLeft, quoteRight := match[4], match[6]
		if quoteLeft != quoteRight {
			return legacyHeredocSpec{}, false, fmt.Errorf("line %d: mismatched heredoc delimiter quotes", lineNumber)
		}
		delimiterToken := match[3] + quoteLeft + match[5] + quoteRight
		return legacyHeredocSpec{indent: match[1], target: match[2], delimiterToken: delimiterToken}, true, nil
	}
	if match := runCatHeredocRedirectPattern.FindStringSubmatch(line); match != nil {
		quoteLeft, quoteRight := match[3], match[5]
		if quoteLeft != quoteRight {
			return legacyHeredocSpec{}, false, fmt.Errorf("line %d: mismatched heredoc delimiter quotes", lineNumber)
		}
		delimiterToken := match[2] + quoteLeft + match[4] + quoteRight
		return legacyHeredocSpec{indent: match[1], target: match[6], delimiterToken: delimiterToken}, true, nil
	}
	return legacyHeredocSpec{}, false, nil
}

func findHeredocEnd(lines []string, start int, delimiter string, stripTabs bool) int {
	for idx := start; idx < len(lines); idx++ {
		line := strings.TrimSuffix(strings.TrimSuffix(lines[idx], "\n"), "\r")
		candidate := line
		if stripTabs {
			candidate = strings.TrimLeft(candidate, "\t")
		}
		if candidate == delimiter {
			return idx
		}
	}
	return -1
}

func splitDockerfileLines(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.SplitAfter(content, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
