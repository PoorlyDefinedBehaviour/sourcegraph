package events_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/hexops/autogold/v2"
	"github.com/sourcegraph/conc"
	"github.com/sourcegraph/log"
	"github.com/sourcegraph/log/logtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sourcegraph/sourcegraph/enterprise/cmd/cody-gateway/internal/events"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

func TestBufferedLogger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("passes events to handler", func(t *testing.T) {
		t.Parallel()

		handler := &mockLogger{}

		// Test with a buffer size of 0, which should immediately submit events
		b := events.NewBufferedLogger(logtest.Scoped(t), handler, 0)
		wg := conc.NewWaitGroup()
		wg.Go(b.Start)

		assert.NoError(t, b.LogEvent(ctx, events.Event{Identifier: "foo"}))
		assert.NoError(t, b.LogEvent(ctx, events.Event{Identifier: "bar"}))
		assert.NoError(t, b.LogEvent(ctx, events.Event{Identifier: "baz"}))

		// Stop the worker and wait for it to finish so that events flush before
		// making any assertions
		b.Stop()
		wg.Wait()

		autogold.Expect([]string{"foo", "bar", "baz"}).Equal(t, asIdentifiersList(handler.ReceivedEvents))

	})

	t.Run("buffers until full", func(t *testing.T) {
		t.Parallel()

		// blockEventSubmissionC should be closed to unblock event submission,
		// until it is all events to handler blocks indefinitely.
		// We have an additional test case to assert that we attempt to submit
		// events directly when the buffer is full, so we have directSubmit as
		// a toggle to stop blocking.
		const blockedEventID = "blocked-submission"
		const bufferFullEventID = "buffer-full"
		blockEventSubmissionC := make(chan struct{})
		handler := &mockLogger{
			PreLogEventHook: func(id string) error {
				if id == blockedEventID {
					<-blockEventSubmissionC // hold up the queue
					return nil
				}
				if id == bufferFullEventID {
					return errors.New("TEST SENTINEL ERROR: 'buffer-full' submitted immediately")
				}
				return nil
			},
		}

		// Assert on our error logging
		l, exportLogs := logtest.Captured(t)

		// Set up a buffered logger we can fill up
		size := 3
		b := events.NewBufferedLogger(l, handler, size)
		wg := conc.NewWaitGroup()
		wg.Go(b.Start)

		// Send an event that will block the queue.
		assert.NoErrorf(t, b.LogEvent(ctx, events.Event{Identifier: blockedEventID}), "event 1 (blocking)")
		// Fill up the buffer with blocked events
		for i := 1; i <= size; i++ {
			assert.NoErrorf(t, b.LogEvent(ctx, events.Event{Identifier: strconv.Itoa(i)}), "event %d (non-blocking)", i)
		}

		// The queue should be full now, directly submit the next event
		err := b.LogEvent(ctx, events.Event{Identifier: bufferFullEventID})
		// Sentinel error indicates we indeed attempted to submit the event directly
		require.Error(t, err)
		autogold.Expect("TEST SENTINEL ERROR: 'buffer-full' submitted immediately").Equal(t, err.Error())

		// Indicate close and stop the worker so that the buffer can flush
		close(blockEventSubmissionC)
		b.Stop()
		wg.Wait()

		// All backlogged events get submitted. Note the "buffer-full" event is
		// submitted "first" because the queue was full when it came in, and
		// we directly submitted it. Then "blocked-submission" is submitted
		// when it is unblocked, and the rest come in order as the queue is
		// flushed.
		autogold.Expect([]string{
			"buffer-full", "blocked-submission", "1", "2",
			"3",
		}).Equal(t, asIdentifiersList(handler.ReceivedEvents))

		// Assert our error logging. There should only be one entry (i.e. no
		// errors on flush).
		errorLogs := exportLogs().Filter(func(l logtest.CapturedLog) bool { return l.Level == log.LevelError })
		autogold.Expect([]string{"failed to queue event within timeout, submitting event directly"}).Equal(t, errorLogs.Messages())
	})

	t.Run("submits events after stop", func(t *testing.T) {
		t.Parallel()

		handler := &mockLogger{}
		l, exportLogs := logtest.Captured(t)
		b := events.NewBufferedLogger(l, handler, 10)
		wg := conc.NewWaitGroup()
		wg.Go(b.Start)

		assert.NoError(t, b.LogEvent(ctx, events.Event{Identifier: "foo"}))

		// Stop the worker and wait for it to finish
		b.Stop()
		wg.Wait()

		// Submit an additional event - this should immediately attempt to
		// submit the event
		assert.NoError(t, b.LogEvent(ctx, events.Event{Identifier: "bar"}))

		// Expect all events to still be logged
		autogold.Expect([]string{"foo", "bar"}).Equal(t, asIdentifiersList(handler.ReceivedEvents))
		// Expect log message to indicate send-after-close
		assert.True(t, exportLogs().Contains(func(l logtest.CapturedLog) bool {
			return strings.Contains(l.Message, "buffer is closed")
		}))
	})
}

type mockLogger struct {
	// PreLogEventHook, if set, is called on LogEvent before the event is added
	// to (*mockLogger).Events.
	PreLogEventHook func(id string) error
	// ReceivedEvents are all the events submitted to LogEvent. When mockLogger
	// is used as the handler for a BufferedLogger, ReceivedEvents must not be
	// accessed until (*BufferedLogger).Stop() has been called and
	// (*BufferedLogger).Start() has exited.
	ReceivedEvents []events.Event
}

func (m *mockLogger) LogEvent(spanCtx context.Context, event events.Event) error {
	var err error
	if m.PreLogEventHook != nil {
		err = m.PreLogEventHook(event.Identifier)
	}
	m.ReceivedEvents = append(m.ReceivedEvents, event)
	return err
}

// asIdentifiersList renders a list of events as a list of their identifiers,
// (events.Event).Identifier, for ease of comparison.
func asIdentifiersList(sources []events.Event) []string {
	var names []string
	for _, s := range sources {
		names = append(names, s.Identifier)
	}
	return names
}
