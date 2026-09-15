package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"ai-business-service/internal/biz"
	"ai-business-service/internal/biz/creations"
	"ai-business-service/internal/data"
	"ai-business-service/internal/worker"

	"github.com/go-kratos/kratos/v3"
	"github.com/go-kratos/kratos/v3/transport/grpc"
	"github.com/go-kratos/kratos/v3/transport/http"
)

func TestNewAppReturnsErrorWhenSchemaInitializationFails(t *testing.T) {
	sentinel := errors.New("index conflict")
	initializer := &fakeSchemaInitializer{err: sentinel}

	app, err := newTestApp(initializer)

	if app != nil {
		t.Fatal("newApp() app = non-nil, want nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("newApp() error = %v, want wrapping %v", err, sentinel)
	}
	if !initializer.called {
		t.Fatal("newApp() did not call schema initializer")
	}
}

func TestNewAppReturnsAppWhenSchemaInitializationSucceeds(t *testing.T) {
	initializer := &fakeSchemaInitializer{}

	app, err := newTestApp(initializer)

	if err != nil {
		t.Fatalf("newApp() error = %v, want nil", err)
	}
	if app == nil {
		t.Fatal("newApp() app = nil, want non-nil")
	}
	if !initializer.called {
		t.Fatal("newApp() did not call schema initializer")
	}
}

func TestNewAppAcceptsCreationUsecaseDependency(t *testing.T) {
	initializer := &fakeSchemaInitializer{}

	app, err := newApp(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		grpc.NewServer(),
		http.NewServer(),
		biz.NewModuleRegistry(),
		nil,
		initializer,
		(*creations.Usecase)(nil),
		(*worker.GenerationSubmissionWorker)(nil),
	)

	if err != nil {
		t.Fatalf("newApp() error = %v, want nil", err)
	}
	if app == nil {
		t.Fatal("newApp() app = nil, want non-nil")
	}
	if !initializer.called {
		t.Fatal("newApp() did not call schema initializer")
	}
}

func newTestApp(initializer data.LocalSchemaInitializer) (*kratos.App, error) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newApp(
		logger,
		grpc.NewServer(),
		http.NewServer(),
		biz.NewModuleRegistry(),
		nil,
		initializer,
		(*creations.Usecase)(nil),
		(*worker.GenerationSubmissionWorker)(nil),
	)
}

type fakeSchemaInitializer struct {
	called bool
	err    error
}

func (initializer *fakeSchemaInitializer) Ensure(context.Context) error {
	initializer.called = true
	return initializer.err
}
