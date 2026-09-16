package dockerfilepreprocess

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	"mvdan.cc/sh/v3/syntax"
)

func splitInstruction(header string) (string, string) {
	header = strings.TrimSpace(header)
	if pos := strings.IndexAny(header, " \t"); pos >= 0 {
		return strings.ToUpper(header[:pos]), strings.TrimSpace(header[pos+1:])
	}
	return strings.ToUpper(header), ""
}

// instructionHeader joins Dockerfile continuations, not shell statements. Keep
// source line ranges separately so pass-through instructions remain byte exact.
func instructionHeader(lines []string, start int, escape byte) (string, int) {
	var header strings.Builder
	for end := start; end < len(lines); end++ {
		line := strings.TrimRight(lines[end], "\r\n")
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			if end == start {
				return line, end
			}
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		n := len(trimmed)
		continued := n > 0 && trimmed[n-1] == escape && (n == 1 || trimmed[n-2] != escape)
		if continued {
			header.WriteString(trimmed[:n-1])
		} else {
			header.WriteString(line)
			return header.String(), end
		}
	}
	return header.String(), len(lines) - 1
}

// Dockerfile flags precede JSON exec arguments and are not shell words.
func jsonCommand(command string) bool {
	if json.Valid([]byte(command)) {
		return true
	}
	if !strings.HasPrefix(command, "--") || !strings.Contains(command, "[") {
		return false
	}
	parsed, err := parser.Parse(strings.NewReader("RUN " + command + "\n"))
	return err == nil && len(parsed.AST.Children) == 1 && parsed.AST.Children[0].Attributes["json"]
}

// instructionHeredocs inspects shell redirection nodes, rather than searching
// text for <<. Quoted strings, here-strings and arithmetic shifts are not
// heredocs. Parsing only the header intentionally leaves heredoc bodies absent;
// the returned partial AST still contains their redirects. Dockerfile body
// boundaries and missing terminators are validated separately.
func instructionHeredocs(command string) ([]parser.Heredoc, error) {
	if !strings.Contains(command, "<<") || jsonCommand(command) {
		return nil, nil
	}
	file, shellErr := syntax.NewParser().Parse(strings.NewReader(command), "")
	var docs []parser.Heredoc
	var parseErr error
	syntax.Walk(file, func(node syntax.Node) bool {
		redirect, ok := node.(*syntax.Redirect)
		if !ok || (redirect.Op != syntax.Hdoc && redirect.Op != syntax.DashHdoc) || redirect.Word == nil {
			return true
		}
		token := redirect.Op.String() + " " + command[redirect.Word.Pos().Offset():redirect.Word.End().Offset()]
		doc, err := parser.ParseHeredoc(token)
		if err != nil {
			parseErr = err
		} else if doc != nil {
			docs = append(docs, *doc)
		}
		return false
	})
	if len(docs) == 0 && shellErr != nil {
		return nil, fmt.Errorf("cannot validate heredoc header: %w; use native COPY heredoc with separate RUN instructions", shellErr)
	}
	return docs, parseErr
}

// COPY/ADD arguments use Dockerfile word syntax, not shell redirections. For
// example, a source filename containing << is not a heredoc operator.
func dockerfileHeredocs(command string) ([]parser.Heredoc, error) {
	if !strings.Contains(command, "<<") || jsonCommand(command) {
		return nil, nil
	}
	lex := shell.NewLex('\\')
	lex.RawQuotes, lex.RawEscapes, lex.SkipUnsetEnv = true, true, true
	words, err := lex.ProcessWords(command, shell.EnvsFromSlice(nil))
	if err != nil {
		return nil, err
	}
	var docs []parser.Heredoc
	for _, word := range words {
		doc, err := parser.ParseHeredoc(word)
		if err != nil {
			return nil, err
		}
		if doc != nil {
			docs = append(docs, *doc)
		}
	}
	return docs, nil
}

// Native RUN <<EOF scripts belong to the selected Dockerfile frontend. Keep
// them intact, including RUN flags; compatibility rewriting is only for cat.
func nativeRunHeredoc(command string) bool {
	lex := shell.NewLex('\\')
	lex.RawQuotes, lex.RawEscapes, lex.SkipUnsetEnv = true, true, true
	words, err := lex.ProcessWords(command, shell.EnvsFromSlice(nil))
	if err != nil {
		return false
	}
	for _, word := range words {
		if strings.HasPrefix(word, "--") {
			continue
		}
		return strings.HasPrefix(word, "<<") && !strings.HasPrefix(word, "<<<")
	}
	return false
}
