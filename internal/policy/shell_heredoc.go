package policy

import "strings"

// Heredocs belong to the command's stdin, not the shell's executable words.
// Keep only the delimiter while lexing the header, then skip each queued body
// at the next newline. Unquoted expansion remains unreadable, rather than
// silently treating executable substitution as literal input.
type shellHeredoc struct {
	delimiter string
	stripTabs bool
	quoted    bool
}

func shellReadHeredoc(runes []rune, start int) (shellHeredoc, int, bool) {
	doc := shellHeredoc{}
	i := start + 2
	if i < len(runes) && runes[i] == '-' {
		doc.stripTabs = true
		i++
	}
	for i < len(runes) && (runes[i] == ' ' || runes[i] == '\t') {
		i++
	}
	if i == len(runes) {
		return doc, 0, false
	}
	if runes[i] == '\'' || runes[i] == '"' {
		body, end, ok := shellReadQuoted(runes, i)
		if !ok || body == "" || shellWordContinues(runes, end) {
			return doc, 0, false
		}
		doc.delimiter, doc.quoted = body, true
		return doc, end, true
	}
	begin := i
	for i < len(runes) && !strings.ContainsRune(" \t\n\r;|&<>", runes[i]) {
		r := runes[i]
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') {
			return doc, 0, false
		}
		i++
	}
	if i == begin {
		return doc, 0, false
	}
	doc.delimiter = string(runes[begin:i])
	return doc, i - 1, true
}

func shellConsumeHeredocs(runes []rune, start int, docs []shellHeredoc) (int, bool) {
	i := start
	for _, doc := range docs {
		found := false
		for i < len(runes) {
			begin := i
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
			line := string(runes[begin:i])
			if i < len(runes) {
				i++
			}
			if doc.stripTabs {
				line = strings.TrimLeft(line, "\t")
			}
			if line == doc.delimiter {
				found = true
				break
			}
			if !doc.quoted && strings.ContainsAny(line, "$`\\") {
				return 0, false
			}
		}
		if !found {
			return 0, false
		}
	}
	return i, true
}
