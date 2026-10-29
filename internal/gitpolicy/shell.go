package gitpolicy

import (
	"errors"
	"strings"
)

// The parser understands enough of Bash and PowerShell to find each command
// in a line, its words after quote removal, and everything it cannot see
// through: expansions, substitutions, subshells and script blocks. It never
// evaluates anything; a word whose value depends on the shell is marked
// dynamic instead.

// word is one argument after quote removal.
type word struct {
	text    string // the value, as far as it is known
	raw     string // the source text
	dynamic bool   // depends on the shell: variables, ~, brace expansion, substitutions
	glob    bool   // has an unquoted * ? or [ that Bash would expand
}

type redirect struct {
	op     string
	target word
}

type heredoc struct {
	delim  string
	quoted bool // the delimiter was quoted, so the body is literal
	strip  bool // <<- strips leading tabs
	body   string
}

// command is one simple command and what the parser found inside it.
type command struct {
	words    []word
	redirs   []redirect
	heredocs []*heredoc
	nested   []*script // substitutions, subshells and script blocks
	subst    bool      // nested holds something whose effect or output is opaque
	sep      string    // what follows: ";", "&&", "||", "|", "&", "\n" or "" at the end
	complex  string    // why the parser cannot vouch for this command
}

func (c *command) empty() bool {
	return len(c.words) == 0 && len(c.redirs) == 0 && len(c.heredocs) == 0 && len(c.nested) == 0 && c.complex == ""
}

type script struct{ cmds []*command }

var errSyntax = errors.New("unbalanced quotes or brackets")

type parser struct {
	src     []rune
	pos     int
	sh      Shell
	pending []*heredoc
}

func parse(src string, sh Shell) (*script, error) {
	p := &parser{src: []rune(src), sh: sh}
	s, err := p.list(0)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() rune { return p.at(0) }

func (p *parser) at(n int) rune {
	if p.pos+n < len(p.src) && p.pos+n >= 0 {
		return p.src[p.pos+n]
	}
	return 0
}

func isBlank(r rune) bool { return r == ' ' || r == '\t' || r == '\r' }

func (p *parser) skipBlanks() {
	for !p.eof() {
		r := p.peek()
		switch {
		case isBlank(r):
			p.pos++
		case r == '\\' && p.sh == Bash && p.at(1) == '\n':
			p.pos += 2
		case r == '`' && p.sh == PowerShell && (p.at(1) == '\n' || p.at(1) == '\r' && p.at(2) == '\n'):
			p.pos += 2
			if p.peek() == '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// list parses commands until stop (a closing bracket) or, for stop 0, the end.
func (p *parser) list(stop rune) (*script, error) {
	s := &script{}
	cur := &command{}
	flush := func(sep string) {
		if !cur.empty() {
			cur.sep = sep
			s.cmds = append(s.cmds, cur)
		}
		cur = &command{}
	}
	for {
		p.skipBlanks()
		if p.eof() {
			if stop != 0 {
				return nil, errSyntax
			}
			flush("")
			return s, nil
		}
		r := p.peek()
		if stop != 0 && r == stop {
			p.pos++
			flush("")
			return s, nil
		}
		switch {
		case r == '\n':
			p.pos++
			flush("\n")
			p.readHeredocs()
		case r == '#':
			for !p.eof() && p.peek() != '\n' {
				p.pos++
			}
		case p.sh == PowerShell && r == '<' && p.at(1) == '#':
			end := strings.Index(string(p.src[p.pos:]), "#>")
			if end < 0 {
				return nil, errSyntax
			}
			p.pos += len([]rune(string(p.src[p.pos:])[:end+2]))
		case r == ';':
			p.pos++
			if p.peek() == ';' {
				p.pos++
			}
			flush(";")
		case r == '&' && p.at(1) == '&':
			p.pos += 2
			flush("&&")
		case r == '&' && p.sh == Bash && p.at(1) == '>':
			if err := p.redirect(cur); err != nil {
				return nil, err
			}
		case r == '&' && p.sh == PowerShell && len(cur.words) == 0:
			p.pos++ // the call operator: what follows is the command
		case r == '&':
			p.pos++
			flush("&")
		case r == '|' && p.at(1) == '|':
			p.pos += 2
			flush("||")
		case r == '|':
			p.pos++
			if p.sh == Bash && p.peek() == '&' {
				p.pos++
			}
			flush("|")
		case r == ',' && p.sh == PowerShell:
			p.pos++ // array separator: the words stay separate arguments
		case r == '(' && p.sh == Bash && p.at(1) != '(' || r == '(' && p.sh == PowerShell:
			p.pos++
			inner, err := p.list(')')
			if err != nil {
				return nil, err
			}
			if p.sh == Bash && len(cur.words) > 0 {
				cur.complex = "defines a function"
			}
			cur.nested = append(cur.nested, inner)
			cur.subst = true
		case r == '(':
			// (( arithmetic )): opaque.
			end, err := p.skipParens()
			if err != nil {
				return nil, err
			}
			cur.complex = "arithmetic"
			cur.words = append(cur.words, word{text: string(p.src[p.pos:end]), raw: string(p.src[p.pos:end]), dynamic: true})
			p.pos = end
		case r == '{' && (p.sh == PowerShell || len(cur.words) == 0 && (isBlank(p.at(1)) || p.at(1) == '\n')):
			p.pos++
			inner, err := p.list('}')
			if err != nil {
				return nil, err
			}
			cur.nested = append(cur.nested, inner)
			cur.subst = true
		case r == ')' || r == '}' && p.sh == PowerShell:
			return nil, errSyntax
		case p.redirectStart():
			if err := p.redirect(cur); err != nil {
				return nil, err
			}
		default:
			w, err := p.word(cur, stop)
			if err != nil {
				return nil, err
			}
			if p.sh == PowerShell && w.raw == "--%" {
				// Stop-parsing: the rest of the line goes to the program as is.
				start := p.pos
				for !p.eof() && p.peek() != '\n' {
					p.pos++
				}
				cur.complex = "uses --%"
				cur.words = append(cur.words, w, word{text: string(p.src[start:p.pos]), raw: string(p.src[start:p.pos]), dynamic: true})
				continue
			}
			cur.words = append(cur.words, w)
		}
	}
}

// skipParens returns the position after the )) closing a (( at pos.
func (p *parser) skipParens() (int, error) {
	depth := 0
	for i := p.pos; i < len(p.src); i++ {
		switch p.src[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i + 1, nil
			}
		}
	}
	return 0, errSyntax
}

func (p *parser) redirectStart() bool {
	r := p.peek()
	if (r == '>' || r == '<') && !(p.sh == Bash && p.at(1) == '(') {
		return true
	}
	if p.sh == PowerShell && r == '*' && p.at(1) == '>' {
		return true
	}
	i := 0
	for p.at(i) >= '0' && p.at(i) <= '9' {
		i++
	}
	return i > 0 && (p.at(i) == '>' || p.at(i) == '<')
}

var redirectOps = []string{"<<<", "<<-", "<<", "<&", "<>", "<", ">>", ">|", ">&", ">"}

func (p *parser) redirect(cur *command) error {
	start := p.pos
	for !p.eof() && (p.peek() >= '0' && p.peek() <= '9' || p.peek() == '&' || p.peek() == '*') {
		p.pos++
	}
	rest := string(p.src[p.pos:min(p.pos+3, len(p.src))])
	op := ""
	for _, o := range redirectOps {
		if strings.HasPrefix(rest, o) {
			op = o
			break
		}
	}
	if op == "" {
		return errSyntax
	}
	p.pos += len(op)
	prefix := string(p.src[start:p.pos])
	p.skipBlanks()
	if strings.HasSuffix(op, "&") && p.sh == PowerShell {
		// 2>&1
		w, err := p.word(cur, 0)
		if err != nil {
			return err
		}
		cur.redirs = append(cur.redirs, redirect{op: prefix, target: w})
		return nil
	}
	if p.eof() || p.peek() == '\n' {
		return errSyntax
	}
	w, err := p.word(cur, 0)
	if err != nil {
		return err
	}
	if p.sh == Bash && (op == "<<" || op == "<<-") {
		h := &heredoc{delim: w.text, quoted: strings.ContainsAny(w.raw, `'"\`), strip: op == "<<-"}
		p.pending = append(p.pending, h)
		cur.heredocs = append(cur.heredocs, h)
		return nil
	}
	if op == "<<<" {
		cur.heredocs = append(cur.heredocs, &heredoc{body: w.text, quoted: !w.dynamic})
		return nil
	}
	cur.redirs = append(cur.redirs, redirect{op: prefix, target: w})
	return nil
}

// readHeredocs reads the bodies of the here-documents opened on the line just
// ended.
func (p *parser) readHeredocs() {
	for _, h := range p.pending {
		var body strings.Builder
		for !p.eof() {
			end := p.pos
			for end < len(p.src) && p.src[end] != '\n' {
				end++
			}
			line := strings.TrimSuffix(string(p.src[p.pos:end]), "\r")
			p.pos = min(end+1, len(p.src))
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.delim {
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		h.body = body.String()
	}
	p.pending = nil
}

func (p *parser) ends(r, stop rune) bool {
	if isBlank(r) || r == '\n' || r == ';' || r == '&' || r == '|' || r == '<' || r == '>' {
		return true
	}
	if r == ')' || stop != 0 && r == stop && p.sh == PowerShell {
		return true
	}
	if p.sh == Bash {
		return r == '('
	}
	return r == ','
}

// word reads one word, attaching substitutions it contains to cur.
func (p *parser) word(cur *command, stop rune) (word, error) {
	start := p.pos
	var w word
	var text, unq strings.Builder
	for !p.eof() {
		r := p.peek()
		procSubst := p.sh == Bash && (r == '<' || r == '>') && p.at(1) == '('
		if p.ends(r, stop) && !procSubst {
			break
		}
		var err error
		if p.sh == Bash {
			err = p.bashPart(cur, &w, &text, &unq)
		} else {
			err = p.psPart(cur, &w, &text, start)
		}
		if err != nil {
			return w, err
		}
	}
	if p.pos == start {
		return w, errSyntax
	}
	w.text = text.String()
	w.raw = string(p.src[start:p.pos])
	if p.sh == Bash && braceExpansion(unq.String()) {
		w.dynamic = true
	}
	return w, nil
}

// braceExpansion reports whether unquoted text has {a,b} or {1..3}.
func braceExpansion(s string) bool {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		return false
	}
	closing := strings.IndexByte(s[open:], '}')
	if closing < 0 {
		return false
	}
	in := s[open : open+closing]
	return strings.Contains(in, ",") || strings.Contains(in, "..")
}

func (p *parser) bashPart(cur *command, w *word, text, unq *strings.Builder) error {
	r := p.peek()
	switch r {
	case '\\':
		switch {
		case p.at(1) == '\n':
			p.pos += 2
		case p.pos+1 >= len(p.src):
			text.WriteRune(r)
			p.pos++
		default:
			text.WriteRune(p.at(1))
			unq.WriteByte('_')
			p.pos += 2
		}
	case '\'':
		end := p.find(p.pos+1, '\'')
		if end < 0 {
			return errSyntax
		}
		text.WriteString(string(p.src[p.pos+1 : end]))
		unq.WriteByte('_')
		p.pos = end + 1
	case '"':
		p.pos++
		for {
			if p.eof() {
				return errSyntax
			}
			c := p.peek()
			if c == '"' {
				p.pos++
				break
			}
			switch c {
			case '\\':
				if strings.ContainsRune("$`\"\\\n", p.at(1)) {
					if p.at(1) != '\n' {
						text.WriteRune(p.at(1))
					}
					p.pos += 2
				} else {
					text.WriteRune(c)
					p.pos++
				}
			case '$':
				if err := p.bashDollar(cur, w, text, true); err != nil {
					return err
				}
			case '`':
				if err := p.backtick(cur, w); err != nil {
					return err
				}
			default:
				text.WriteRune(c)
				p.pos++
			}
		}
		unq.WriteByte('_')
	case '$':
		return p.bashDollar(cur, w, text, false)
	case '`':
		return p.backtick(cur, w)
	case '<', '>':
		// <( ) or >( ) process substitution
		if p.at(1) == '(' {
			p.pos += 2
			inner, err := p.list(')')
			if err != nil {
				return err
			}
			cur.nested = append(cur.nested, inner)
			cur.subst = true
			w.dynamic = true
			return nil
		}
		text.WriteRune(r)
		p.pos++
	default:
		switch r {
		case '*', '?', '[':
			w.glob = true
		case '~':
			if text.Len() == 0 {
				w.dynamic = true
			}
		}
		text.WriteRune(r)
		unq.WriteRune(r)
		p.pos++
	}
	return nil
}

// find returns the index of the next r at or after i, or -1.
func (p *parser) find(i int, r rune) int {
	for ; i < len(p.src); i++ {
		if p.src[i] == r {
			return i
		}
	}
	return -1
}

func isNameStart(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isNameChar(r rune) bool { return isNameStart(r) || r >= '0' && r <= '9' }

func (p *parser) bashDollar(cur *command, w *word, text *strings.Builder, quoted bool) error {
	start := p.pos
	next := p.at(1)
	switch {
	case next == '(' && p.at(2) == '(':
		p.pos++
		end, err := p.skipParens()
		if err != nil {
			return err
		}
		p.pos = end
		cur.subst = true
		w.dynamic = true
	case next == '(':
		p.pos += 2
		inner, err := p.list(')')
		if err != nil {
			return err
		}
		if lit, ok := literalCat(inner); ok {
			text.WriteString(lit)
			return nil
		}
		cur.nested = append(cur.nested, inner)
		cur.subst = true
		w.dynamic = true
	case next == '{':
		end := p.find(p.pos+2, '}')
		if end < 0 {
			return errSyntax
		}
		p.pos = end + 1
		w.dynamic = true
	case !quoted && (next == '\'' || next == '"'):
		end := p.pos + 2
		for end < len(p.src) && p.src[end] != next {
			if p.src[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(p.src) {
			return errSyntax
		}
		p.pos = end + 1
		w.dynamic = true
	case isNameStart(next):
		p.pos++
		for !p.eof() && isNameChar(p.peek()) {
			p.pos++
		}
		w.dynamic = true
	case next >= '0' && next <= '9' || strings.ContainsRune("@*#?$!-", next) && next != 0:
		p.pos += 2
		w.dynamic = true
	default:
		text.WriteRune('$')
		p.pos++
		return nil
	}
	text.WriteString(string(p.src[start:p.pos]))
	return nil
}

func (p *parser) backtick(cur *command, w *word) error {
	var body strings.Builder
	i := p.pos + 1
	for ; i < len(p.src) && p.src[i] != '`'; i++ {
		if p.src[i] == '\\' && i+1 < len(p.src) && strings.ContainsRune("`\\$", p.src[i+1]) {
			i++
		}
		body.WriteRune(p.src[i])
	}
	if i >= len(p.src) {
		return errSyntax
	}
	p.pos = i + 1
	inner, err := parse(body.String(), Bash)
	if err != nil {
		return err
	}
	cur.nested = append(cur.nested, inner)
	cur.subst = true
	w.dynamic = true
	return nil
}

// literalCat recognises $(cat <<'EOF' ... EOF), the usual way to pass a
// multi-line commit message: its value is the here-document's body, so it is
// a plain word rather than an opaque substitution.
func literalCat(s *script) (string, bool) {
	if len(s.cmds) != 1 {
		return "", false
	}
	c := s.cmds[0]
	if len(c.words) != 1 || c.words[0].raw != "cat" || len(c.redirs) > 0 || len(c.heredocs) != 1 || len(c.nested) > 0 || c.complex != "" {
		return "", false
	}
	h := c.heredocs[0]
	if !h.quoted && strings.ContainsAny(h.body, "$`\\") {
		return "", false
	}
	return strings.TrimRight(h.body, "\n"), true
}

func isSingleQuote(r rune) bool {
	return r == '\'' || r == '‘' || r == '’' || r == '‚' || r == '‛'
}

func isDoubleQuote(r rune) bool { return r == '"' || r == '“' || r == '”' || r == '„' }

func psEscape(r rune) rune {
	switch r {
	case '0':
		return 0
	case 'a':
		return '\a'
	case 'b':
		return '\b'
	case 'e':
		return 0x1b
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'v':
		return '\v'
	}
	return r
}

func (p *parser) psPart(cur *command, w *word, text *strings.Builder, start int) error {
	r := p.peek()
	switch {
	case r == '`':
		if p.pos+1 >= len(p.src) {
			return errSyntax
		}
		text.WriteRune(psEscape(p.at(1)))
		p.pos += 2
	case isSingleQuote(r):
		p.pos++
		for {
			if p.eof() {
				return errSyntax
			}
			c := p.peek()
			if isSingleQuote(c) {
				if isSingleQuote(p.at(1)) {
					text.WriteRune(c)
					p.pos += 2
					continue
				}
				p.pos++
				break
			}
			text.WriteRune(c)
			p.pos++
		}
	case isDoubleQuote(r):
		p.pos++
		for {
			if p.eof() {
				return errSyntax
			}
			c := p.peek()
			if isDoubleQuote(c) {
				if isDoubleQuote(p.at(1)) {
					text.WriteRune(c)
					p.pos += 2
					continue
				}
				p.pos++
				break
			}
			switch c {
			case '`':
				text.WriteRune(psEscape(p.at(1)))
				p.pos += 2
			case '$':
				if err := p.psDollar(cur, w, text); err != nil {
					return err
				}
			default:
				text.WriteRune(c)
				p.pos++
			}
		}
	case r == '$':
		return p.psDollar(cur, w, text)
	case r == '@' && p.pos == start:
		next := p.at(1)
		switch {
		case next == '(':
			p.pos += 2
			inner, err := p.list(')')
			if err != nil {
				return err
			}
			cur.nested = append(cur.nested, inner)
			cur.subst = true
			w.dynamic = true
		case isSingleQuote(next) || isDoubleQuote(next):
			return p.hereString(cur, w, text)
		case next == '{':
			p.pos += 2
			inner, err := p.list('}')
			if err != nil {
				return err
			}
			cur.nested = append(cur.nested, inner)
			cur.subst = true
			w.dynamic = true
		case isNameStart(next):
			p.pos++
			for !p.eof() && isNameChar(p.peek()) {
				p.pos++
			}
			w.dynamic = true // splatting
		default:
			text.WriteRune(r)
			p.pos++
		}
	default:
		switch r {
		case '~':
			if text.Len() == 0 {
				w.dynamic = true
			}
		case '{', '}', '(':
			w.dynamic = true
		}
		text.WriteRune(r)
		p.pos++
	}
	return nil
}

func (p *parser) psDollar(cur *command, w *word, text *strings.Builder) error {
	start := p.pos
	next := p.at(1)
	switch {
	case next == '(':
		p.pos += 2
		inner, err := p.list(')')
		if err != nil {
			return err
		}
		cur.nested = append(cur.nested, inner)
		cur.subst = true
	case next == '{':
		end := p.find(p.pos+2, '}')
		if end < 0 {
			return errSyntax
		}
		p.pos = end + 1
	case isNameStart(next) || next == '?' || next == '^' || next == '$':
		p.pos += 2
		for !p.eof() && (isNameChar(p.peek()) || p.peek() == ':') {
			p.pos++
		}
	default:
		text.WriteRune('$')
		p.pos++
		return nil
	}
	w.dynamic = true
	text.WriteString(string(p.src[start:p.pos]))
	return nil
}

// hereString reads @'...'@ or @"..."@, which start and end on their own lines.
func (p *parser) hereString(cur *command, w *word, text *strings.Builder) error {
	double := isDoubleQuote(p.at(1))
	i := p.pos + 2
	for i < len(p.src) && isBlank(p.src[i]) {
		i++
	}
	if i >= len(p.src) || p.src[i] != '\n' {
		return errSyntax
	}
	i++
	bodyStart := i
	for {
		if i >= len(p.src) {
			return errSyntax
		}
		lineEnd := i
		for lineEnd < len(p.src) && p.src[lineEnd] != '\n' {
			lineEnd++
		}
		if lineEnd-i >= 2 && p.src[i+1] == '@' && (double && isDoubleQuote(p.src[i]) || !double && isSingleQuote(p.src[i])) {
			body := strings.TrimSuffix(strings.TrimSuffix(string(p.src[bodyStart:max(bodyStart, i-1)]), "\r"), "\n")
			if double && strings.ContainsAny(body, "$`") {
				w.dynamic = true
				if strings.Contains(body, "$(") {
					cur.subst = true
				}
			}
			text.WriteString(body)
			p.pos = i + 2
			return nil
		}
		i = lineEnd + 1
	}
}
