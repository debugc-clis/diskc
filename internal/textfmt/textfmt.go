package textfmt

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

type Writer struct {
	writer io.Writer
	err    error
}

func NewWriter(writer io.Writer) *Writer { return &Writer{writer: writer} }

func (writer *Writer) Write(data []byte) (int, error) {
	if writer.err != nil {
		return 0, writer.err
	}
	written, err := writer.writer.Write(data)
	if err != nil {
		writer.err = err
	}
	return written, err
}

func (writer *Writer) Err() error { return writer.err }

func Safe(value string) string {
	var result strings.Builder
	for _, character := range value {
		switch character {
		case '\n':
			result.WriteString(`\n`)
		case '\r':
			result.WriteString(`\r`)
		case '\t':
			result.WriteString(`\t`)
		default:
			if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
				if character <= 0xff {
					fmt.Fprintf(&result, `\x%02x`, character)
				} else {
					fmt.Fprintf(&result, `\u%04x`, character)
				}
				continue
			}
			result.WriteRune(character)
		}
	}
	return result.String()
}
