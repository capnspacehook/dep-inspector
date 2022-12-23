package main

import (
	"bufio"
	"io"
)

type lineReader struct {
	*bufio.Scanner
	line int
}

func newLineReader(r io.Reader) *lineReader {
	return &lineReader{
		Scanner: bufio.NewScanner(r),
	}
}

func (l *lineReader) Scan() bool {
	l.line++
	return l.Scanner.Scan()
}

func (l *lineReader) Line() int {
	return l.line
}

func getSrcLines(l *lineReader, startLine, endLine int) ([]string, error) {
	srcLines := make([]string, 0, 1)

	handleLine := func(line int) bool {
		if line == startLine {
			srcLines = append(srcLines, l.Text())
		}
		if line == endLine {
			if startLine != endLine {
				srcLines = append(srcLines, l.Text())
			}
			return false
		}
		if line > startLine && line < endLine {
			srcLines = append(srcLines, l.Text())
		}

		return true
	}

	// handle the buffered line first in case the desired line is
	// already buffered
	if !handleLine(l.Line()) {
		return srcLines, nil
	}
	for l.Scan() {
		if !handleLine(l.Line()) {
			return srcLines, nil
		}
	}
	if err := l.Err(); err != nil {
		return nil, err
	}

	return srcLines, nil
}
