package ingest

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// FileChange is one file write extracted from a tool_use.
type FileChange struct {
	Tool, FilePath, Op    string
	AddedChars, RemovedCh int
	Excerpt               string
	ToolUseID             string
}

const maxExcerpt = 300

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ExtractFileChanges returns the file changes made by one tool_use (name and
// raw JSON input): Edit, MultiEdit, Write, NotebookEdit, and Bash commands
// that write files (sed -i, > path, >> path, tee path).
func ExtractFileChanges(name string, input []byte) []FileChange {
	switch name {
	case "Edit":
		var in struct {
			FilePath  string `json:"file_path"`
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
			return nil
		}
		return []FileChange{{Tool: name, FilePath: in.FilePath, Op: "edit",
			AddedChars: utf8.RuneCountInString(in.NewString), RemovedCh: utf8.RuneCountInString(in.OldString),
			Excerpt: firstRunes(in.NewString, maxExcerpt)}}
	case "MultiEdit":
		var in struct {
			FilePath string `json:"file_path"`
			Edits    []struct {
				OldString string `json:"old_string"`
				NewString string `json:"new_string"`
			} `json:"edits"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
			return nil
		}
		fc := FileChange{Tool: name, FilePath: in.FilePath, Op: "edit"}
		var news []string
		for _, e := range in.Edits {
			fc.AddedChars += utf8.RuneCountInString(e.NewString)
			fc.RemovedCh += utf8.RuneCountInString(e.OldString)
			news = append(news, e.NewString)
		}
		fc.Excerpt = firstRunes(strings.Join(news, "\n"), maxExcerpt)
		return []FileChange{fc}
	case "Write":
		var in struct {
			FilePath string `json:"file_path"`
			Content  string `json:"content"`
		}
		if json.Unmarshal(input, &in) != nil || in.FilePath == "" {
			return nil
		}
		return []FileChange{{Tool: name, FilePath: in.FilePath, Op: "write",
			AddedChars: utf8.RuneCountInString(in.Content), Excerpt: firstRunes(in.Content, maxExcerpt)}}
	case "NotebookEdit":
		var in struct {
			Path      string `json:"notebook_path"`
			NewSource string `json:"new_source"`
		}
		if json.Unmarshal(input, &in) != nil || in.Path == "" {
			return nil
		}
		return []FileChange{{Tool: name, FilePath: in.Path, Op: "edit",
			AddedChars: utf8.RuneCountInString(in.NewSource), Excerpt: firstRunes(in.NewSource, maxExcerpt)}}
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(input, &in) != nil || in.Command == "" {
			return nil
		}
		var out []FileChange
		for _, p := range ShellTargets(in.Command) {
			out = append(out, FileChange{Tool: name, FilePath: p, Op: "shell", Excerpt: firstRunes(in.Command, maxExcerpt)})
		}
		return out
	}
	return nil
}

type shTok struct {
	text   string
	redir  bool // an unquoted > or >> (the next word is the target)
	skip   bool // an fd redirect such as 2> or >&2, or < (the next word is not a target)
	sep    bool // command separator (; | & && || newline)
	quoted bool // the word contained quotes or escapes
}

// shLex tokenizes a whole shell command: quotes (which may span newlines),
// backslash escapes, separators, redirections and heredocs. A heredoc body
// (<<'EOF' ... EOF) is skipped: its lines are data, not commands. It is
// deliberately conservative, not a shell parser.
func shLex(cmd string) []shTok {
	var toks []shTok
	var cur strings.Builder
	has, quoted := false, false
	flush := func() {
		if has {
			toks = append(toks, shTok{text: cur.String(), quoted: quoted})
		}
		cur.Reset()
		has, quoted = false, false
	}
	var pending []string // heredoc delimiters awaiting their body
	rs := []rune(cmd)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case c == '\'':
			has, quoted = true, true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				cur.WriteRune(rs[i])
			}
		case c == '"':
			has, quoted = true, true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				cur.WriteRune(rs[i])
			}
		case c == '\\' && i+1 < len(rs):
			i++
			has, quoted = true, true
			cur.WriteRune(rs[i])
		case c == '\n':
			flush()
			toks = append(toks, shTok{sep: true})
			if len(pending) > 0 {
				i = skipHeredocs(rs, i+1, pending) - 1
				pending = nil
			}
		case c == ' ' || c == '\t':
			flush()
		case c == '<' && i+1 < len(rs) && rs[i+1] == '<' && !(i+2 < len(rs) && rs[i+2] == '<'):
			flush()
			j := i + 2
			if j < len(rs) && rs[j] == '-' {
				j++
			}
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			var d strings.Builder
			for ; j < len(rs) && !strings.ContainsRune(" \t\n;|&)", rs[j]); j++ {
				if !strings.ContainsRune(`'"\\`, rs[j]) {
					d.WriteRune(rs[j])
				}
			}
			pending = append(pending, d.String())
			i = j - 1
		case c == '<':
			flush()
			toks = append(toks, shTok{redir: true, skip: true})
			if i+1 < len(rs) && rs[i+1] == '<' { // <<< here-string
				i++
				for i+1 < len(rs) && rs[i+1] == '<' {
					i++
				}
			}
		case c == ';' || c == '|' || c == '&' && !(!has && i+1 < len(rs) && rs[i+1] == '>'):
			flush()
			for i+1 < len(rs) && rs[i+1] == c {
				i++
			}
			toks = append(toks, shTok{sep: true})
		case c == '>' || c == '&' && i+1 < len(rs) && rs[i+1] == '>':
			skip := false
			if c == '&' { // &> file: both streams, not a plain write target
				skip = true
				i++
			}
			if has {
				if strings.Trim(cur.String(), "0123456789") == "" {
					skip = true // 2> / 1> style fd redirect
					cur.Reset()
					has, quoted = false, false
				} else {
					flush()
				}
			}
			if i+1 < len(rs) && rs[i+1] == '>' {
				i++
			}
			if i+1 < len(rs) && rs[i+1] == '&' { // >&2
				skip = true
				i++
			}
			toks = append(toks, shTok{redir: true, skip: skip})
		default:
			has = true
			cur.WriteRune(c)
		}
	}
	flush()
	return toks
}

// skipHeredocs returns the index just after the bodies of the pending
// heredocs, which start at rs[from].
func skipHeredocs(rs []rune, from int, delims []string) int {
	i := from
	for _, d := range delims {
		for i < len(rs) {
			j := i
			for j < len(rs) && rs[j] != '\n' {
				j++
			}
			line := strings.TrimSpace(string(rs[i:j]))
			i = j
			if i < len(rs) {
				i++
			}
			if line == d {
				break
			}
		}
	}
	return i
}

// plausibleTarget accepts only literal path-looking words: they contain a
// '/' or a '.', no variable, glob or redirection characters, and no spaces
// unless the word was quoted.
func plausibleTarget(p string, quoted bool) bool {
	if p == "" || strings.HasPrefix(p, "-") || strings.HasPrefix(p, "&") || strings.HasPrefix(p, "/dev/") {
		return false
	}
	if !strings.ContainsAny(p, "/.") {
		return false
	}
	if strings.ContainsAny(p, "$`*?<>|;") {
		return false
	}
	if !quoted && strings.ContainsAny(p, "(){} \t\n") {
		return false
	}
	if strings.ContainsAny(p, "\n") {
		return false
	}
	return true
}

// ShellTargets returns the files a shell command writes via sed -i, >, >> or
// tee. Commands without a literal target path yield nothing.
func ShellTargets(cmd string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t shTok) {
		if plausibleTarget(t.text, t.quoted) && !seen[t.text] {
			seen[t.text] = true
			out = append(out, t.text)
		}
	}
	var seg []shTok
	run := func() {
		shTargets(seg, add)
		seg = nil
	}
	for _, t := range shLex(cmd) {
		if t.sep {
			run()
			continue
		}
		seg = append(seg, t)
	}
	run()
	return out
}

func shTargets(seg []shTok, add func(shTok)) {
	// redirections anywhere in the simple command
	for i := 0; i < len(seg); i++ {
		if seg[i].redir && i+1 < len(seg) && !seg[i+1].redir {
			if !seg[i].skip {
				add(seg[i+1])
			}
			i++
		}
	}
	// words without redirection operators and their targets
	var words []shTok
	for i := 0; i < len(seg); i++ {
		if seg[i].redir {
			i++
			continue
		}
		words = append(words, seg[i])
	}
	for len(words) > 0 && (words[0].text == "sudo" || words[0].text == "command" || words[0].text == "time" ||
		!words[0].quoted && strings.Contains(words[0].text, "=") && !strings.HasPrefix(words[0].text, "-")) {
		words = words[1:]
	}
	if len(words) == 0 {
		return
	}
	switch filepath.Base(words[0].text) {
	case "sed":
		inplace, eflag := false, false
		var args []shTok
		skipNext := false
		ws := words[1:]
		for i := 0; i < len(ws); i++ {
			w := ws[i].text
			if skipNext {
				skipNext = false
				continue
			}
			switch {
			case w == "-i":
				inplace = true
				// BSD sed: `-i ''` takes an empty backup suffix as its own word
				if i+1 < len(ws) && ws[i+1].text == "" && ws[i+1].quoted {
					i++
				}
			case strings.HasPrefix(w, "-i") || w == "--in-place" || strings.HasPrefix(w, "--in-place="):
				inplace = true
			case w == "-e" || w == "-f":
				eflag = true
				skipNext = true
			case strings.HasPrefix(w, "-") && len(w) > 1:
				if !strings.HasPrefix(w, "--") && strings.Contains(w[1:], "i") {
					inplace = true
				}
			default:
				args = append(args, ws[i])
			}
		}
		if !inplace {
			return
		}
		files := args
		if !eflag {
			if len(args) < 2 {
				return
			}
			files = args[1:]
		}
		for _, f := range files {
			add(f)
		}
	case "tee":
		for _, w := range words[1:] {
			if strings.HasPrefix(w.text, "-") {
				continue
			}
			add(w)
		}
	}
}
