// Package yamlite parses the small YAML subset stream-analyzer's config uses:
// block mappings and sequences (including "- key: value" items), plain,
// single- and double-quoted scalars, flow sequences of scalars ("[a, b]") and
// comments. Scalars come back as strings, "~"/"null"/empty values as nil.
// Anything outside the subset (flow mappings, block scalars, anchors, tags)
// is an error rather than a silent misparse.
package yamlite

import (
	"fmt"
	"strconv"
	"strings"
)

type line struct {
	num    int // 1-based
	indent int
	text   string // without indentation, comment or trailing space
}

type parser struct {
	lines []line
	pos   int
}

// Parse parses a document whose top level is a mapping.
func Parse(data []byte) (map[string]any, error) {
	ls, err := tokenize(string(data))
	if err != nil {
		return nil, err
	}
	if len(ls) == 0 {
		return map[string]any{}, nil
	}
	if ls[0].indent != 0 {
		return nil, errorf(ls[0], "document must start at column 1")
	}
	p := &parser{lines: ls}
	v, err := p.block(0)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("yaml: top level must be a mapping")
	}
	if p.pos < len(p.lines) {
		return nil, errorf(p.lines[p.pos], "unexpected content")
	}
	return m, nil
}

func errorf(l line, format string, args ...any) error {
	return fmt.Errorf("yaml line %d: %s", l.num, fmt.Sprintf(format, args...))
}

func tokenize(doc string) ([]line, error) {
	var out []line
	for i, raw := range strings.Split(doc, "\n") {
		raw = strings.TrimRight(raw, "\r")
		body := strings.TrimLeft(raw, " ")
		l := line{num: i + 1, indent: len(raw) - len(body)}
		if strings.HasPrefix(body, "\t") {
			return nil, errorf(l, "tabs are not allowed in indentation")
		}
		l.text = strings.TrimRight(stripComment(body), " \t")
		if l.text == "" || l.indent == 0 && (l.text == "---" || l.text == "...") {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// stripComment removes a # comment that starts the line or follows
// whitespace, ignoring # inside quotes. A quote opens a quoted scalar only
// where a scalar can start (after whitespace, '[' or ','): the apostrophe
// in a plain value such as Media Team's SSD quotes nothing.
func stripComment(s string) string {
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '"' && c == '\\':
			i++
		case quote == '\'' && c == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++ // '' is a quote inside a single-quoted scalar
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\'') && (i == 0 || strings.IndexByte(" \t[,", s[i-1]) >= 0):
			quote = c
		case quote == 0 && c == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

func isSeqItem(text string) bool { return text == "-" || strings.HasPrefix(text, "- ") }

func (p *parser) block(indent int) (any, error) {
	if isSeqItem(p.lines[p.pos].text) {
		return p.seq(indent)
	}
	return p.mapping(indent)
}

func (p *parser) mapping(indent int) (map[string]any, error) {
	m := map[string]any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, errorf(l, "unexpected indentation")
		}
		if isSeqItem(l.text) {
			return nil, errorf(l, "list item where a key was expected")
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, errorf(l, "expected \"key: value\"")
		}
		if _, dup := m[key]; dup {
			return nil, errorf(l, "duplicate key %q", key)
		}
		p.pos++
		v, err := p.value(rest, indent, l)
		if err != nil {
			return nil, err
		}
		m[key] = v
	}
	return m, nil
}

// value parses what follows "key:": an inline scalar, or a nested block on
// the following lines (a sequence may sit at the key's own indentation).
func (p *parser) value(rest string, indent int, l line) (any, error) {
	if rest != "" {
		return scalar(rest, l)
	}
	if p.pos < len(p.lines) {
		next := p.lines[p.pos]
		if next.indent > indent {
			return p.block(next.indent)
		}
		if next.indent == indent && isSeqItem(next.text) {
			return p.seq(indent)
		}
	}
	return nil, nil
}

func (p *parser) seq(indent int) ([]any, error) {
	s := []any{}
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if l.indent < indent || l.indent == indent && !isSeqItem(l.text) {
			break
		}
		if l.indent > indent {
			return nil, errorf(l, "unexpected indentation")
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		switch _, _, isKey := splitKey(rest); {
		case rest == "":
			p.pos++
			v, err := p.value("", indent, l)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
		case isKey:
			// "- key: value" starts a mapping whose keys line up with "key".
			col := l.indent + len(l.text) - len(rest)
			p.lines[p.pos] = line{num: l.num, indent: col, text: rest}
			m, err := p.mapping(col)
			if err != nil {
				return nil, err
			}
			s = append(s, m)
		default:
			p.pos++
			v, err := scalar(rest, l)
			if err != nil {
				return nil, err
			}
			s = append(s, v)
		}
	}
	return s, nil
}

// splitKey splits "key: value" or "key:"; quoted and flow values are never
// keys.
func splitKey(text string) (key, rest string, ok bool) {
	if text == "" || strings.ContainsRune(`"'[{`, rune(text[0])) {
		return "", "", false
	}
	if k, r, found := strings.Cut(text, ": "); found {
		return strings.TrimSpace(k), strings.TrimSpace(r), true
	}
	if k, found := strings.CutSuffix(text, ":"); found {
		return strings.TrimSpace(k), "", true
	}
	return "", "", false
}

func scalar(s string, l line) (any, error) {
	switch s[0] {
	case '"', '\'':
		v, rest, err := quoted(s)
		if err != nil {
			return nil, errorf(l, "%v", err)
		}
		if strings.TrimSpace(rest) != "" {
			return nil, errorf(l, "unexpected text after quoted string: %q", rest)
		}
		return v, nil
	case '[':
		return flowSeq(s, l)
	case '{':
		return nil, errorf(l, "flow mappings ({...}) are not supported")
	case '|', '>':
		return nil, errorf(l, "block scalars (| and >) are not supported")
	case '&', '*', '!':
		return nil, errorf(l, "anchors, aliases and tags are not supported")
	}
	if s == "~" || s == "null" {
		return nil, nil
	}
	return s, nil
}

// quoted parses a quoted string at the start of s and returns the rest.
func quoted(s string) (string, string, error) {
	q := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case q == '"' && s[i] == '\\':
			i++
		case s[i] == q && q == '\'' && i+1 < len(s) && s[i+1] == '\'':
			i++ // '' is an escaped single quote
		case s[i] == q:
			if q == '\'' {
				return strings.ReplaceAll(s[1:i], "''", "'"), s[i+1:], nil
			}
			v, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", fmt.Errorf("bad double-quoted string %s", s[:i+1])
			}
			return v, s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("unterminated %c-quoted string", q)
}

func flowSeq(s string, l line) ([]any, error) {
	inner, ok := strings.CutSuffix(s[1:], "]")
	if !ok {
		return nil, errorf(l, "unterminated flow sequence")
	}
	out := []any{}
	for rest := strings.TrimSpace(inner); rest != ""; {
		var item string
		if rest[0] == '"' || rest[0] == '\'' {
			v, after, err := quoted(rest)
			if err != nil {
				return nil, errorf(l, "%v", err)
			}
			item, rest = v, strings.TrimSpace(after)
			if rest != "" && rest[0] != ',' {
				return nil, errorf(l, "expected ',' in flow sequence")
			}
		} else {
			item, rest, _ = strings.Cut(rest, ",")
			item = strings.TrimSpace(item)
			if strings.ContainsAny(item, "[]{}") {
				return nil, errorf(l, "nested flow collections are not supported")
			}
			rest = "," + rest // uniform separator handling below
		}
		out = append(out, item)
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ","))
	}
	return out, nil
}
