package errors

import (
	"fmt"
	"strings"
)

type CompileError struct {
	File    string
	Line    int
	Col     int
	Phase   string
	Message string
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("%s:%d:%d: %s error: %s", e.File, e.Line, e.Col, e.Phase, e.Message)
}

func (e *CompileError) FormatWithSource(source string) string {
	lines := strings.Split(source, "\n")
	result := e.Error() + "\n"

	if e.Line >= 1 && e.Line <= len(lines) {
		srcLine := lines[e.Line-1]
		result += fmt.Sprintf("    %s\n", srcLine)
		if e.Col >= 1 && e.Col <= len(srcLine)+1 {
			result += fmt.Sprintf("    %s^\n", strings.Repeat(" ", e.Col-1))
		}
	}

	return result
}

type ErrorList struct {
	Errors []*CompileError
	Limit  int
}

func NewErrorList(limit int) *ErrorList {
	return &ErrorList{Limit: limit}
}

func (el *ErrorList) Add(err *CompileError) {
	if el.Limit > 0 && len(el.Errors) >= el.Limit {
		return
	}
	el.Errors = append(el.Errors, err)
}

func (el *ErrorList) HasErrors() bool {
	return len(el.Errors) > 0
}

func (el *ErrorList) Error() string {
	if len(el.Errors) == 0 {
		return ""
	}
	msg := ""
	for _, e := range el.Errors {
		msg += e.Error() + "\n"
	}
	return msg
}

func (el *ErrorList) FormatWithSource(source string) string {
	if len(el.Errors) == 0 {
		return ""
	}
	msg := ""
	for _, e := range el.Errors {
		msg += e.FormatWithSource(source)
	}
	return msg
}
