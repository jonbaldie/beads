package tracker

import "fmt"

func (e *Engine) msg(format string, args ...interface{}) {
	if e.OnMessage != nil {
		e.OnMessage(fmt.Sprintf(format, args...))
	}
}

func (e *Engine) warn(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	e.warnings = append(e.warnings, msg)
	if e.OnWarning != nil {
		e.OnWarning(msg)
	}
}
