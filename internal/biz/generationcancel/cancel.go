// Package generationcancel owns the user-requested B2B cancellation
// transition.  It is separate from creations because the persistence boundary
// needs generation's frozen provider submission facts; putting that dependency
// back into creations would create an import cycle.
package generationcancel

import (
	"context"
	"errors"
	"strings"
	"time"

	"ai-business-service/internal/biz/shared"
)

var (
	// ErrInvalidCommand is a local shape error and must not touch storage.
	ErrInvalidCommand = errors.New("generation cancel: invalid command")
	// ErrDependenciesUnavailable keeps an incompletely wired handler
	// fail-closed rather than fabricating a cancellation success.
	ErrDependenciesUnavailable = errors.New("generation cancel: dependencies are unavailable")
	// ErrNotFound intentionally covers both a missing creation and a non-owner
	// request so the HTTP layer cannot become an ownership oracle.
	ErrNotFound = errors.New("generation cancel: creation not found")
	// ErrNotReady means the request has not reached an auditable provider
	// submission intent or bound external job.  The caller may retry after the
	// submission state has durably advanced; no local terminal is manufactured.
	ErrNotReady = errors.New("generation cancel: creation is not ready to cancel")
	// ErrConflict represents a terminal/publication/state race.  It is distinct
	// from ErrNotReady because retrying it blindly can change the outcome.
	ErrConflict = errors.New("generation cancel: creation state conflict")
)

const maxIdentifierLength = 512

// Command contains only caller identity and the Go creation identity.  It has
// no provider/job fields: all provider routing must be loaded from frozen
// persistence facts inside the transaction.
type Command struct {
	CreationID string
	UserID     string
	At         time.Time
}

func (command Command) Validate() error {
	if !validIdentifier(command.CreationID) || !validIdentifier(command.UserID) {
		return ErrInvalidCommand
	}
	return nil
}

// Result is an acceptance receipt, not a cancelled terminal.  A zero event
// count is valid when a prepared submission intent exists but its provider job
// has not yet been observed; reconciliation will create the step event once it
// binds that frozen job.
type Result struct {
	CreationID       string
	Accepted         bool
	CancelEventCount int
	Replayed         bool
}

// Store performs the owner/state/job verification plus creation-state and
// cancel-outbox mutations using the transaction context provided by Usecase.
// It has no ledger, refund or terminal-writing capability.
type Store interface {
	RequestCancellation(context.Context, Command) (Result, error)
}

type Usecase struct {
	store Store
	tx    shared.TxRunner
	now   func() time.Time
}

func NewUsecase(store Store, tx shared.TxRunner) *Usecase {
	return NewUsecaseWithClock(store, tx, time.Now)
}

func NewUsecaseWithClock(store Store, tx shared.TxRunner, clock func() time.Time) *Usecase {
	if clock == nil {
		clock = time.Now
	}
	return &Usecase{store: store, tx: tx, now: clock}
}

// Cancel freezes the business timestamp before opening the transaction.  The
// Mongo runner may retry its callback, so persisting a fresh time in each
// retry would make an otherwise idempotent request nondeterministic.
func (usecase *Usecase) Cancel(ctx context.Context, command Command) (Result, error) {
	if usecase == nil || usecase.store == nil || usecase.tx == nil || usecase.now == nil {
		return Result{}, ErrDependenciesUnavailable
	}
	if err := command.Validate(); err != nil {
		return Result{}, err
	}
	command.At = usecase.now().UTC()
	if command.At.IsZero() {
		return Result{}, ErrInvalidCommand
	}
	var result Result
	if err := usecase.tx.WithinTx(ctx, func(txCtx context.Context) error {
		var err error
		result, err = usecase.store.RequestCancellation(txCtx, command)
		return err
	}); err != nil {
		return Result{}, err
	}
	return result, nil
}

func validIdentifier(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, " \t\r\n") && len(value) <= maxIdentifierLength
}
