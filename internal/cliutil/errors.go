package cliutil

import "errors"

type UsageError struct {
	Err error
}

func (err UsageError) Error() string { return err.Err.Error() }
func (err UsageError) Unwrap() error { return err.Err }

func Usage(err error) error {
	if err == nil {
		return nil
	}
	return UsageError{Err: err}
}

func IsUsage(err error) bool {
	var usageError UsageError
	return errors.As(err, &usageError)
}
