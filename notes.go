package main

import (
	"fmt"
	"reflect"
	"strings"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"gopkg.in/yaml.v3"
)

func cleanNotesForSave(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' {
			return '\n'
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	return strings.TrimRight(s, "\n")
}

func sanitizeNoteInput(s string) []rune {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []rune
	for _, r := range s {
		switch {
		case r == '\n':
			out = append(out, '\n')
		case r == '\t':
			out = append(out, ' ', ' ')
		case unicode.IsControl(r):
			continue
		default:
			out = append(out, r)
		}
	}
	return out
}

func setServerNotes(target ServerConfig, notes string) (*Config, error) {
	notes = cleanNotesForSave(notes)
	return editServersConfig(func(cfg *Config, servers *yaml.Node) error {
		index := target.sourceIndex
		var current *ServerConfig
		for i := range cfg.Servers {
			if cfg.Servers[i].sourceIndex == index {
				current = &cfg.Servers[i]
				break
			}
		}
		if current == nil || !reflect.DeepEqual(*current, target) || index < 0 || index >= len(servers.Content) {
			return fmt.Errorf("服务器配置已变化，请退出后重新打开")
		}
		entry := servers.Content[index]
		if entry.Kind == yaml.AliasNode || entry.Anchor != "" {
			// Override locally while leaving the shared anchor unchanged.
			entry = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!merge", Value: "<<"}, entry,
			}}
			servers.Content[index] = entry
		}
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("服务器配置必须为 YAML 映射")
		}
		hasMerge := mappingLookup(entry, "notes") != nil
		for i := 0; i < len(entry.Content); i += 2 {
			if entry.Content[i].Value == "notes" {
				if notes == "" {
					// Check whether a merged base mapping still supplies non-empty notes.
					entry.Content = append(entry.Content[:i], entry.Content[i+2:]...)
					if inherited := unwrapAlias(mappingLookup(entry, "notes")); inherited != nil && inherited.Value != "" {
						entry.Content = append(entry.Content,
							&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "notes"},
							&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""},
						)
					}
					return nil
				}
				value := *entry.Content[i+1]
				value.Kind, value.Tag, value.Value = yaml.ScalarNode, "!!str", notes
				value.Style, value.Alias, value.Content, value.Anchor = 0, nil, nil, ""
				if strings.Contains(notes, "\n") {
					value.Style = yaml.LiteralStyle
				}
				entry.Content[i+1] = &value
				return nil
			}
		}
		if notes == "" && !hasMerge {
			return nil
		}
		style := yaml.Style(0)
		if strings.Contains(notes, "\n") {
			style = yaml.LiteralStyle
		}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "notes"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Style: style, Value: notes},
		)
		return nil
	})
}

func noteLineBounds(buf []rune, pos int) (lineStart, lineEnd, colWidth int) {
	pos = max(0, min(pos, len(buf)))
	lineStart = pos
	for lineStart > 0 && buf[lineStart-1] != '\n' {
		lineStart--
	}
	lineEnd = pos
	for lineEnd < len(buf) && buf[lineEnd] != '\n' {
		lineEnd++
	}
	colWidth = runewidth.StringWidth(string(buf[lineStart:pos]))
	return lineStart, lineEnd, colWidth
}

func notePosAtColumn(buf []rune, lineStart, lineEnd, targetWidth int) int {
	w := 0
	for i := lineStart; i < lineEnd; i++ {
		rw := runewidth.RuneWidth(buf[i])
		if w+rw > targetWidth {
			return i
		}
		w += rw
	}
	return lineEnd
}

func moveNoteCursorVertical(buf []rune, pos, dir int) int {
	start, end, colW := noteLineBounds(buf, pos)
	if dir < 0 {
		if start == 0 {
			return 0
		}
		prevEnd := start - 1
		prevStart := prevEnd
		for prevStart > 0 && buf[prevStart-1] != '\n' {
			prevStart--
		}
		return notePosAtColumn(buf, prevStart, prevEnd, colW)
	}
	if end >= len(buf) {
		return len(buf)
	}
	nextStart := end + 1
	nextEnd := nextStart
	for nextEnd < len(buf) && buf[nextEnd] != '\n' {
		nextEnd++
	}
	return notePosAtColumn(buf, nextStart, nextEnd, colW)
}

func wrapNoteLines(text string, width int) []string {
	if width <= 0 {
		return nil
	}
	rawLines := strings.Split(text, "\n")
	var out []string
	for _, line := range rawLines {
		if line == "" {
			out = append(out, "")
			continue
		}
		var cur strings.Builder
		curW := 0
		for _, r := range line {
			rw := max(1, runewidth.RuneWidth(r))
			if curW+rw > width && curW > 0 {
				out = append(out, cur.String())
				cur.Reset()
				curW = 0
			}
			if rw <= width {
				cur.WriteRune(r)
				curW += rw
			}
		}
		out = append(out, cur.String())
	}
	return out
}

func renderNoteEditorLines(buf []rune, pos, width, height int, textStyle, cursorStyle lipgloss.Style) []string {
	if width <= 0 || height <= 0 {
		return nil
	}
	pos = max(0, min(pos, len(buf)))
	cursorW := 1
	if width == 1 {
		cursorW = 0
	}

	type visualLine struct {
		text      string
		hasCursor bool
		before    string
		cursorCh  string
		after     string
	}
	var lines []visualLine
	cursorLine := 0

	var beforeCur, afterCur strings.Builder
	curW := 0
	inCursorLine := false
	var cursorChar string

	flush := func() {
		if inCursorLine {
			cursorLine = len(lines)
			ch := cursorChar
			if ch == "" {
				ch = " "
			}
			lines = append(lines, visualLine{
				hasCursor: true,
				before:    beforeCur.String(),
				cursorCh:  ch,
				after:     afterCur.String(),
			})
		} else {
			lines = append(lines, visualLine{text: beforeCur.String()})
		}
		beforeCur.Reset()
		afterCur.Reset()
		curW = 0
		inCursorLine = false
		cursorChar = ""
	}

	for i := 0; i <= len(buf); i++ {
		if i == pos {
			if i < len(buf) && buf[i] != '\n' {
				rw := max(1, runewidth.RuneWidth(buf[i]))
				if curW+rw > width && curW > 0 {
					flush()
				}
				inCursorLine = true
				cursorChar = string(buf[i])
				curW += rw
				continue
			}
			if curW+cursorW > width && curW > 0 {
				flush()
			}
			inCursorLine = true
			cursorChar = " "
			curW += cursorW
		}
		if i == len(buf) {
			break
		}
		r := buf[i]
		if r == '\n' {
			flush()
			continue
		}
		rw := max(1, runewidth.RuneWidth(r))
		if curW+rw > width && curW > 0 {
			flush()
		}
		if inCursorLine {
			afterCur.WriteRune(r)
		} else {
			beforeCur.WriteRune(r)
		}
		curW += rw
	}
	flush()

	start := 0
	if cursorLine >= height {
		start = cursorLine - height + 1
	}
	if start+height > len(lines) {
		start = max(0, len(lines)-height)
	}
	end := min(len(lines), start+height)

	out := make([]string, 0, height)
	for _, vl := range lines[start:end] {
		if vl.hasCursor {
			line := textStyle.Render(vl.before) + cursorStyle.Render(vl.cursorCh) + textStyle.Render(vl.after)
			out = append(out, line)
		} else {
			out = append(out, textStyle.Render(vl.text))
		}
	}
	return out
}
