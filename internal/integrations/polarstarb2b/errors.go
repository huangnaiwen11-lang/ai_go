package polarstarb2b

import (
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotSent proves a local validation/context failure before HTTP dispatch.
	ErrNotSent = errors.New("polarstar b2b: request not sent")
	// ErrOutcomeUnknown never authorizes a refund, a replacement key or POST replay.
	ErrOutcomeUnknown = errors.New("polarstar b2b: outcome unknown")
	ErrConflict       = errors.New("polarstar b2b: conflict")
)

// ClientError contains only bounded local classifications. Supplier messages,
// URLs, response bodies and transport error strings are deliberately discarded.
type ClientError struct {
	HTTPStatus int
	Code       string
	RetryAfter time.Duration
	kind       error
	contextErr error
}

func (e *ClientError) Error() string {
	return fmt.Sprintf("polarstar b2b: %s (status=%d)", e.Code, e.HTTPStatus)
}

func (e *ClientError) Is(target error) bool {
	return target == e.kind || (target == ErrConflict && e.HTTPStatus == 409) ||
		(e.contextErr != nil && target == e.contextErr)
}

func notSent(code string, cause error) error {
	return &ClientError{Code: code, kind: ErrNotSent, contextErr: cause}
}
