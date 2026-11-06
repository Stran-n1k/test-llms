// Package validation describes a rejected caller input field by field.
package validation

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTextLength is the maximum length of a free-text field in characters.
const MaxTextLength = 200

type (
	// FieldError is one problem with one input field.
	FieldError struct {
		Field   string
		Message string
	}

	// Error reports that caller input was rejected. It always holds at least
	// one field.
	Error struct {
		Fields []FieldError
	}
)

// Error implements error.
func (e *Error) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		parts = append(parts, field.Field+": "+field.Message)
	}

	return "validation failed: " + strings.Join(parts, "; ")
}

// Collector accumulates field problems so a caller sees all of them at once.
type Collector struct {
	fields []FieldError
}

// Add records a problem with a field.
func (c *Collector) Add(field, message string) {
	c.fields = append(c.fields, FieldError{Field: field, Message: message})
}

// Err returns the accumulated problems, or nil when there are none.
func (c *Collector) Err() error {
	if len(c.fields) == 0 {
		return nil
	}

	return &Error{Fields: c.fields}
}

// NormalizeText trims the surrounding whitespace of a required free-text field
// and records a problem when the result is not acceptable.
func (c *Collector) NormalizeText(field, value string) string {
	value = strings.TrimSpace(value)

	switch {
	case value == "":
		c.Add(field, "is required")
	case utf8.RuneCountInString(value) > MaxTextLength:
		c.Add(field, fmt.Sprintf("must be at most %d characters", MaxTextLength))
	case strings.ContainsFunc(value, unicode.IsControl):
		c.Add(field, "must not contain control characters")
	}

	return value
}
