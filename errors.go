package main

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
)

// wsError wraps an error with a message and a captured stack trace.
// Use Wrap, Wrapf, or Errorf to create instances.
type wsError struct {
	message string
	cause   error
	*stack
}

// Wrap wraps err with msg and captures a stack trace. Returns nil if err is nil.
// If err is already a *wsError, its stack trace is reused.
func Wrap(err error, msg string) error {
	if err == nil {
		return nil
	}
	e := &wsError{
		message: msg,
		cause:   err,
	}
	var existing *wsError
	if errors.As(err, &existing) && existing.stack != nil {
		e.stack = existing.stack
	} else {
		e.stack = callers(3)
	}
	return e
}

// Wrapf wraps err with a formatted message and captures a stack trace.
// Returns nil if err is nil.
func Wrapf(err error, format string, args ...any) error {
	if err == nil {
		return nil
	}
	e := &wsError{
		message: fmt.Sprintf(format, args...),
		cause:   err,
	}
	var existing *wsError
	if errors.As(err, &existing) && existing.stack != nil {
		e.stack = existing.stack
	} else {
		e.stack = callers(3)
	}
	return e
}

// Errorf creates a new error with a formatted message and captures a stack trace.
func Errorf(format string, args ...any) error {
	return &wsError{
		message: fmt.Sprintf(format, args...),
		stack:   callers(3),
	}
}

func (e *wsError) Error() string {
	if e.cause == nil {
		return e.message
	}
	return e.message + ": " + e.cause.Error()
}

func (e *wsError) Unwrap() error { return e.cause }

// Format implements fmt.Formatter. With %+v, the stack trace is appended.
func (e *wsError) Format(s fmt.State, verb rune) {
	switch verb {
	case 'v':
		if s.Flag('+') {
			io.WriteString(s, e.Error())
			if e.stack != nil {
				e.stack.Format(s, verb)
			}
			return
		}
		fallthrough
	case 's', 'q':
		io.WriteString(s, e.Error())
	}
}

// ---------------------------------------------------------------------------
// Stack trace machinery (adapted from alvin/backend/internal/util/stack.go)
// ---------------------------------------------------------------------------

type frame uintptr

func (f frame) pc() uintptr { return uintptr(f) - 1 }

func (f frame) file() string {
	fn := runtime.FuncForPC(f.pc())
	if fn == nil {
		return "unknown"
	}
	file, _ := fn.FileLine(f.pc())
	return file
}

func (f frame) line() int {
	fn := runtime.FuncForPC(f.pc())
	if fn == nil {
		return 0
	}
	_, line := fn.FileLine(f.pc())
	return line
}

func (f frame) name() string {
	fn := runtime.FuncForPC(f.pc())
	if fn == nil {
		return "unknown"
	}
	return fn.Name()
}

func (f frame) Format(s fmt.State, verb rune) {
	switch verb {
	case 's':
		switch {
		case s.Flag('+'):
			io.WriteString(s, f.name())
			io.WriteString(s, "\n\t")
			io.WriteString(s, f.file())
		default:
			io.WriteString(s, lastSegment(f.file()))
		}
	case 'd':
		io.WriteString(s, strconv.Itoa(f.line()))
	case 'n':
		io.WriteString(s, funcname(f.name()))
	case 'v':
		f.Format(s, 's')
		io.WriteString(s, ":")
		f.Format(s, 'd')
	}
}

const appModulePrefix = "github.com/vinayreddy/weatherstation"

func isAppFrame(f frame) bool {
	name := f.name()
	return name != "unknown" && strings.HasPrefix(name, appModulePrefix)
}

type stack []uintptr

func (st *stack) Format(s fmt.State, verb rune) {
	if verb == 'v' && s.Flag('+') {
		for _, pc := range *st {
			f := frame(pc)
			if !isAppFrame(f) {
				continue
			}
			fmt.Fprintf(s, "\n%+v", f)
		}
	}
}

func callers(skip int) *stack {
	const depth = 32
	var pcs [depth]uintptr
	n := runtime.Callers(skip, pcs[:])
	var st stack = pcs[0:n]
	return &st
}

func funcname(name string) string {
	i := strings.LastIndex(name, "/")
	name = name[i+1:]
	i = strings.Index(name, ".")
	if i >= 0 {
		return name[i+1:]
	}
	return name
}

func lastSegment(path string) string {
	i := strings.LastIndex(path, "/")
	if i >= 0 {
		return path[i+1:]
	}
	return path
}
