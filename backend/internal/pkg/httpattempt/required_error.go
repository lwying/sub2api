package httpattempt

import "errors"

type RequiredAuditError struct {
	Cause error
}

func (e *RequiredAuditError) Error() string {
	return "required request audit metadata is unavailable"
}

func (e *RequiredAuditError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func IsRequiredAuditError(err error) bool {
	var target *RequiredAuditError
	return errors.As(err, &target)
}
