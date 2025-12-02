package testutil

import (
	"context"
	"testing"

	dcontext "github.com/docker/distribution/context"
	"github.com/sirupsen/logrus"
)

type logWriterType struct {
	t testing.TB
}

func (l logWriterType) Write(p []byte) (n int, err error) {
	l.t.Log(string(p))
	return len(p), nil
}

type opts func(l *logrus.Entry)

func WithLogLevel(ll string) func(l *logrus.Entry) {
	return func(l *logrus.Entry) {
		lll, err := logrus.ParseLevel(ll)
		if err != nil {
			lll = logrus.DebugLevel
		}
		l.Logger.Level = lll
	}
}

func NewContextWithLogger(tb testing.TB, opts ...opts) context.Context {
	ctx := context.Background()

	logger := NewTestLogger(tb, opts...)
	ctx = dcontext.WithLogger(ctx, logger)

	return ctx
}

func NewTestLogger(t testing.TB, opts ...opts) dcontext.Logger {
	logger := logrus.New().WithFields(
		logrus.Fields{
			"test": true,
		},
	)
	logger.Logger.Level = logrus.DebugLevel
	logger.Logger.SetOutput(logWriterType{t: t})

	for _, opt := range opts {
		opt(logger)
	}

	return logger
}
