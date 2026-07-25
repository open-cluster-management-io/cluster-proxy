package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRunMain(t *testing.T) {
	tests := []struct {
		name         string
		execute      func() error
		wantExitCode int
		wantStderr   string
		wantFlushes  int
	}{
		{
			name:         "success",
			execute:      func() error { return nil },
			wantExitCode: 0,
			wantFlushes:  1,
		},
		{
			name:         "error",
			execute:      func() error { return errors.New("boom") },
			wantExitCode: 1,
			wantStderr:   "boom\n",
			wantFlushes:  1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr strings.Builder
			flushes := 0

			exitCode := runMain(test.execute, &stderr, func() {
				flushes++
			})

			if exitCode != test.wantExitCode {
				t.Errorf("exit code = %d, want %d", exitCode, test.wantExitCode)
			}
			if stderr.String() != test.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), test.wantStderr)
			}
			if flushes != test.wantFlushes {
				t.Errorf("flush count = %d, want %d", flushes, test.wantFlushes)
			}
		})
	}
}

func TestSignalContextPreservesParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, stop := newSignalContext(parent)
	defer stop()

	cancelParent()

	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("signal context did not propagate parent cancellation")
	}
}
