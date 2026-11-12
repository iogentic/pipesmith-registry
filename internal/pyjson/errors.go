package pyjson

import (
	"errors"
	"fmt"
)

// SyntaxError is Python's json.JSONDecodeError. Pos is a position in code
// points of the decoded text, as Python counts it.
type SyntaxError struct {
	Msg    string
	Pos    int
	Lineno int
	Colno  int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("%s: line %d column %d (char %d)", e.Msg, e.Lineno, e.Colno, e.Pos)
}

// Error is any other exception the json module raises. Class is the name of
// the Python class: UnicodeDecodeError, UnicodeEncodeError, ValueError,
// RecursionError or TypeError. Message is str() of the exception.
type Error struct {
	Class   string
	Message string
}

func (e *Error) Error() string {
	return e.Message
}

// IsValueError reports whether err is one Python counts as a ValueError:
// JSONDecodeError, UnicodeDecodeError, UnicodeEncodeError and ValueError.
func IsValueError(err error) bool {
	var syntax *SyntaxError
	if errors.As(err, &syntax) {
		return true
	}
	var other *Error
	if errors.As(err, &other) {
		switch other.Class {
		case "UnicodeDecodeError", "UnicodeEncodeError", "ValueError":
			return true
		}
	}
	return false
}
