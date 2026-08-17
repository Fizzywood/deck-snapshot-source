package restore

import "context"

// Rebooter is the only reboot authority used by restore. It is deliberately
// typed, fixed and testable: callers cannot supply commands, arguments or a
// destination. Preflight must succeed before any production mutation.
type Rebooter interface {
	Preflight(context.Context) error
	Request(context.Context) error
	BootID(context.Context) (string, error)
}

func NewSessionRebooter() Rebooter { return sessionRebooter{} }

func validBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character == '-' {
				continue
			}
			return false
		}
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}
