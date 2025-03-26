package main

import (
	"path/filepath"
	"slices"
	"strings"
)

type editorSyntax struct {
	// fileType is the name of the file type to display.
	fileType string
	// matchers contains patterns to match against the file name.
	matchers []string
	keywords []string
	// singleLineCommentStart contains the character(s) that a single-line
	// comment starts with.
	singleLineCommentStart string
	multilineCommentStart  string
	multilineCommentEnd    string

	flags int
}

const (
	enableNumberHighlight = 1 << iota
	enableStringHighlight
)

var highlightDB = []editorSyntax{
	{
		fileType: "go",
		matchers: []string{".go"},
		keywords: []string{
			"switch", "if", "for", "range", "break", "continue", "return", "else", "case",
			"struct", "type",

			"int|", "int32|", "int64|",
			"uint|", "uint32|", "uint64|",
			"float|", "float32|", "float64|",
			"string|",
			"rune|",
			"byte|",
			"map|",
			"chan|",
			"error|",
			"func|",
		},
		singleLineCommentStart: "//",
		multilineCommentStart:  "/*",
		multilineCommentEnd:    "*/",
		flags:                  enableNumberHighlight | enableStringHighlight,
	},
}

const (
	highlightNormal = iota
	highlightComment
	highlightMultiComment
	highlightKeyword1
	highlightKeyword2
	highlightString
	highlightNumber
	highlightMatch
)

type editorHighlight int

var searchHighlightLine int
var beforeSearchHighlights []editorHighlight

func editorSelectSyntaxHighlight() {
	e.syntax = nil
	if e.filename == "" {
		return
	}

	ext := filepath.Ext(e.filename)

outer:
	for _, syntax := range highlightDB {
		for _, matcher := range syntax.matchers {
			if matcher[0] == '.' {
				if matcher == ext {
					e.syntax = &syntax
					break outer
				}
			} else if strings.Contains(e.filename, matcher) {
				e.syntax = &syntax
				break outer
			}
		}
	}

	for _, row := range e.row {
		editorUpdateSyntax(&row)
	}
}

func editorUpdateSyntax(row *editorRow) {
	row.highlight = slices.Grow(row.highlight, len(row.render))
	row.highlight = row.highlight[:len(row.render)]

	if e.syntax == nil {
		return
	}

	isPrevSep := true
	var stringStart rune = 0
	isInComment := row.idx > 0 && e.row[row.idx-1].hasOpenComment

	i := 0
outer:
	for i < len(row.render) {
		ch := rune(row.render[i]) // TODO: multi-byte character support

		var prevHl editorHighlight = highlightNormal
		if i > 0 {
			prevHl = row.highlight[i-1]
		}

		lineCommentStart := e.syntax.singleLineCommentStart
		if len(lineCommentStart) > 0 && stringStart == 0 && !isInComment {
			if strings.HasPrefix(row.render[i:], lineCommentStart) {
				for j := i; j < len(row.highlight); j++ {
					row.highlight[j] = highlightComment
				}
				break
			}
		}

		multilineCommentStart := e.syntax.multilineCommentStart
		multilineCommentEnd := e.syntax.multilineCommentEnd
		if len(multilineCommentStart) > 0 && len(multilineCommentEnd) > 0 && stringStart == 0 {
			if isInComment {
				row.highlight[i] = highlightMultiComment
				if strings.HasPrefix(row.render[i:], multilineCommentEnd) {
					for j := range len(multilineCommentEnd) {
						row.highlight[i+j] = highlightMultiComment
					}
					i += len(multilineCommentEnd)
					isInComment = false
					isPrevSep = true
					continue
				} else {
					i++
					continue
				}
			} else if strings.HasPrefix(row.render[i:], multilineCommentStart) {
				for j := range len(multilineCommentStart) {
					row.highlight[i+j] = highlightMultiComment
				}
				i += len(multilineCommentStart)
				isInComment = true
				continue
			}
		}

		if e.syntax.flags&enableStringHighlight != 0 {
			if stringStart != 0 {
				row.highlight[i] = highlightString
				if ch == '\\' && i+1 < len(row.render) {
					row.highlight[i+1] = highlightString
					i += 2
					continue
				}
				if ch == stringStart { // this is the closing quote
					stringStart = 0
				}
				i++
				isPrevSep = true
				continue
			} else {
				if ch == '"' || ch == '\'' {
					stringStart = ch
					row.highlight[i] = highlightString
					i++
					continue
				}
			}
		}

		if e.syntax.flags&enableNumberHighlight != 0 {
			if (ch >= '0' && ch <= '9' && (isPrevSep || prevHl == highlightNumber)) ||
				(ch == '.' && prevHl == highlightNumber) {
				row.highlight[i] = highlightNumber
				isPrevSep = false
				i++
				continue
			}
		}

		keywords := e.syntax.keywords
		if isPrevSep {
			for _, keywordPattern := range keywords {
				isSecondary := strings.HasSuffix(keywordPattern, "|")
				keyword := strings.TrimSuffix(keywordPattern, "|")

				if strings.HasPrefix(row.render[i:], keyword) {
					end := i + len(keyword)
					matchedKeyword := false
					if end < len(row.render) && isSeparator(rune(row.render[end])) {
						matchedKeyword = true
					}
					if !matchedKeyword && end == len(row.render) {
						matchedKeyword = true
					}

					if matchedKeyword {
						var highlight editorHighlight = highlightKeyword1
						if isSecondary {
							highlight = highlightKeyword2
						}

						for j := i; j < len(row.render); j++ {
							row.highlight[j] = highlight
						}
						i += len(keyword)
						isPrevSep = false
						continue outer
					}
				}
			}
		}

		row.highlight[i] = highlightNormal

		isPrevSep = isSeparator(ch)

		i++
	}

	changed := isInComment != row.hasOpenComment
	row.hasOpenComment = isInComment
	if changed && row.idx+1 < len(e.row) {
		editorUpdateSyntax(&e.row[row.idx+1])
	}
}

func editorSyntaxToColour(hl editorHighlight) int {
	switch hl {
	case highlightComment, highlightMultiComment:
		return 36 // cyan
	case highlightKeyword1:
		return 33 // yellow
	case highlightKeyword2:
		return 32 // green
	case highlightString:
		return 35 // magenta
	case highlightNumber:
		return 31 // red
	case highlightMatch:
		return 34 // blue
	default:
		return 37 // white
	}
}

func isSeparator(ch rune) bool {
	return ch == ' ' || ch == 0 || strings.Contains(",.()+-/*=~%<>[];", string(ch))
}

func highlightSearchResult(row editorRow, query string, offset int) {
	searchHighlightLine = row.idx
	beforeSearchHighlights = slices.Clone(row.highlight)
	for i := range len(query) {
		row.highlight[i+offset] = highlightMatch
	}
}

func clearSearchHighlight(rows []editorRow) {
	if len(beforeSearchHighlights) > 0 {
		copy(rows[searchHighlightLine].highlight, beforeSearchHighlights)
		beforeSearchHighlights = nil
	}
}
