// Package admin holds the rules of the administration changes and the table
// of answers (decision A1 of the phase 6 plan). A rule reads the records it
// needs and returns nil or a refusal; it writes nothing. Answer turns a
// refusal into the status and the body of the contract.
package admin

import (
	"net/http"
	"strings"
)

// Refusal is one refusal of a change. Field is the dot path into the
// request body, "*" for the request as a whole, or empty where the answer
// has no field. Area names the area of a 501.
type Refusal struct {
	Code    Code
	Field   string
	Message string
	Area    string
}

func (r *Refusal) Error() string { return r.Message }

// Refusals are the refusals of one request. They travel together, so that
// the answer 400 lists every field.
type Refusals []*Refusal

func (l Refusals) Error() string {
	messages := make([]string, 0, len(l))
	for _, r := range l {
		if r != nil {
			messages = append(messages, r.Message)
		}
	}
	return strings.Join(messages, "; ")
}

// Unwrap lets errors.As and errors.Is find each refusal of the list.
func (l Refusals) Unwrap() []error {
	list := make([]error, len(l))
	for i, r := range l {
		list[i] = r
	}
	return list
}

// err is the list as an error, nil when the list is empty.
func (l Refusals) err() error {
	if len(l) == 0 {
		return nil
	}
	return l
}

// FieldMessage is one object of the list of a 400 answer.
type FieldMessage struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// Message is the body {"message"}.
type Message struct {
	Message string `json:"message"`
}

// AreaMessage is the body {"message", "area"} of a 501.
type AreaMessage struct {
	Message string `json:"message"`
	Area    string `json:"area"`
}

// internalError is the message of the answer 500, which names no detail.
const internalError = "internal error"

// Answer gives the status and the body of the answer to err: a refusal, a
// list of refusals, or an error that wraps them (fmt.Errorf with %w,
// errors.Join). Field refusals answer 400 with one object per refusal in
// their order, at "*" where a refusal names no field. A refusal of another
// status wins over field refusals; the first such refusal is answered
// alone. Where the contract fixes the message of a code, the answer has
// that message. nil, an error that is or wraps anything but refusals, a
// code without a row and a refusal of a 501 without an area answer 500.
func Answer(err error) (int, any) {
	var list []*Refusal
	if !collect(err, &list) {
		return http.StatusInternalServerError, Message{internalError}
	}
	for _, r := range list {
		if a, ok := answers[r.Code]; !ok || a.form == formMessageArea && r.Area == "" {
			return http.StatusInternalServerError, Message{internalError}
		}
	}
	for _, r := range list {
		a := answers[r.Code]
		switch a.form {
		case formMessage:
			return a.status, Message{a.messageOf(r)}
		case formMessageArea:
			return a.status, AreaMessage{Message: a.messageOf(r), Area: r.Area}
		}
	}
	fields := make([]FieldMessage, len(list))
	for i, r := range list {
		field := r.Field
		if field == "" {
			field = "*"
		}
		fields[i] = FieldMessage{ID: field, Message: answers[r.Code].messageOf(r)}
	}
	return answers[list[0].Code].status, fields
}

func (a answer) messageOf(r *Refusal) string {
	if a.message != "" {
		return a.message
	}
	return r.Message
}

// collect adds the refusals of err to list. It reports false when err is
// nil, holds no refusal, or holds an error that is no refusal.
func collect(err error, list *[]*Refusal) bool {
	switch e := err.(type) {
	case *Refusal:
		if e == nil {
			return false
		}
		*list = append(*list, e)
		return true
	case interface{ Unwrap() []error }:
		return collectAll(e.Unwrap(), list)
	case interface{ Unwrap() error }:
		return collect(e.Unwrap(), list)
	}
	return false
}

func collectAll(errs []error, list *[]*Refusal) bool {
	if len(errs) == 0 {
		return false
	}
	for _, e := range errs {
		if !collect(e, list) {
			return false
		}
	}
	return true
}
