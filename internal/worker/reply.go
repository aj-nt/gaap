package worker

import "strings"

// Action kinds returned by parseReply.
const (
	actionCmd  = "cmd"
	actionDone = "done"
	actionFail = "fail"
)

// protocolTokens are the reply prefixes the worker protocol defines, in the
// order they are looked for.
var protocolTokens = []struct{ kind, prefix string }{
	{actionCmd, "CMD:"},
	{actionDone, "DONE:"},
	{actionFail, "FAIL:"},
}

// leakedMarkup are the tool-call delimiters a model sometimes appends to the
// text it was asked to emit as plain protocol. They are not part of the
// command and must not reach the filter or the shell.
var leakedMarkup = []string{
	"</parameter>", "</invoke>", "</function_calls>", "</antml:function_calls>",
	"</antml:invoke>", "</tool_calls>", "</function>", "</result>", "</output>",
	"</br>", "<br>", "<br/>", "<br />",
	"<parameter name=\"command\">", "<parameter name=\"cmd\">",
	"<parameter>", "</｜DSML｜",
}

// parseReply extracts the protocol action from a model reply.
//
// The reply is the model's plain text, but a model that has been trained to
// emit tool calls will wrap its answer in its own markup and add commentary
// around it. Every turn spent refusing `go list ./... | head -100</parameter>`
// for the trailing "</" was a turn that could not be spent on the task, and
// every "let me look at the structure first" preamble was a turn that produced
// nothing. Both are the same defect: the action is in there, buried.
func parseReply(text string) (kind, body string, ok bool) {
	clean := stripLeakedMarkup(text)
	offset := 0

	for _, rawLine := range strings.SplitAfter(clean, "\n") {
		line := strings.TrimLeft(rawLine, " \t\r")
		start := offset + len(rawLine) - len(line)

		for _, tok := range protocolTokens {
			if !strings.HasPrefix(line, tok.prefix) {
				continue
			}
			rest := clean[start+len(tok.prefix):]
			if tok.kind == actionCmd {
				return actionCmd, commandFrom(rest), true
			}
			// DONE and FAIL bodies are prose and may run to several lines.
			return tok.kind, strings.TrimSpace(rest), true
		}

		offset += len(rawLine)
	}
	return "", "", false
}

// replyHadMarkup reports whether the reply carried leaked tool markup, so the
// executor can say so in the log instead of silently rewriting the command.
func replyHadMarkup(text string) bool {
	return stripLeakedMarkup(text) != text
}

// stripLeakedMarkup removes the known tool-call delimiters, plus any tag whose
// name contains a non-ASCII rune (the model's own sentinel, e.g. "｜DSML").
// It does not touch ordinary HTML or quoted text: a tag it does not recognise
// is left exactly where it was, because `grep '</div>' page.html` is a
// legitimate command and this function is not the place to guess.
func stripLeakedMarkup(s string) string {
	out := s
	for _, tok := range leakedMarkup {
		out = strings.ReplaceAll(out, tok, "")
	}

	var b strings.Builder
	for i := 0; i < len(out); {
		if out[i] != '<' {
			b.WriteByte(out[i])
			i++
			continue
		}
		end := strings.IndexByte(out[i:], '>')
		if end < 0 {
			b.WriteString(out[i:])
			break
		}
		inner := out[i+1 : i+end]
		if !isASCII(inner) {
			i += end + 1
			continue
		}
		b.WriteString(out[i : i+end+1])
		i += end + 1
	}
	return b.String()
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7e {
			return false
		}
	}
	return true
}

// commandFrom takes the command out of the text after "CMD:". A command is one
// line — the protocol says so and the filter checks one line — except for a
// heredoc, whose body is data and runs to its terminator.
func commandFrom(rest string) string {
	lines := strings.Split(rest, "\n")
	first := cutUnterminatedTag(strings.TrimSpace(lines[0]))
	if !strings.Contains(first, "<<") {
		return first
	}

	delim := heredocDelimiter(first)
	if delim == "" {
		return first
	}

	var b strings.Builder
	b.WriteString(first)
	for _, line := range lines[1:] {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(line, "\r"))
		if strings.TrimSpace(line) == delim {
			break
		}
	}
	return b.String()
}

// cutUnterminatedTag drops a trailing "</..." with no closing ">" — the shape
// of a truncated sentinel like "</｜DSML｜" — which would otherwise be read as a
// redirect into a file named by the markup. It only looks at closing-tag
// fragments, so a heredoc operator ("<<") is never mistaken for one.
func cutUnterminatedTag(line string) string {
	idx := strings.LastIndex(line, "</")
	if idx < 0 || strings.Contains(line[idx:], ">") {
		return line
	}
	return strings.TrimSpace(line[:idx])
}

// heredocDelimiter returns the word after a heredoc operator, unquoted, or ""
// if the line has none.
func heredocDelimiter(line string) string {
	idx := strings.Index(line, "<<")
	if idx < 0 {
		return ""
	}
	j := idx + 2
	if j < len(line) && line[j] == '-' {
		j++
	}
	for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
		j++
	}
	if j >= len(line) || line[j] == '<' {
		return ""
	}

	var delim strings.Builder
	var quote byte
	for j < len(line) {
		c := line[j]
		if quote != 0 {
			if c == quote {
				quote = 0
				j++
				continue
			}
			delim.WriteByte(c)
			j++
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			j++
			continue
		}
		if c == ' ' || c == '\t' || c == ';' || c == '|' || c == '&' || c == '>' {
			break
		}
		delim.WriteByte(c)
		j++
	}
	return delim.String()
}
