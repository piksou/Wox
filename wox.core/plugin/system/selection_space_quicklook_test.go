package system

import (
	"testing"
	"time"

	"wox/util/keyboard"
)

// quickLookFake replaces the named quick look window with counters and an
// optional open blocker so races between keyup and open completion can be
// exercised deterministically.
type quickLookFake struct {
	now        time.Time
	blockOpen  chan struct{}
	openResult bool
	opened     int
	closed     int
	workerDone chan struct{}
}

func newQuickLookFake(start time.Time) *quickLookFake {
	return &quickLookFake{now: start, openResult: true, workerDone: make(chan struct{})}
}

func (f *quickLookFake) advance(d time.Duration) { f.now = f.now.Add(d) }

func newQuickLookTestState(fake *quickLookFake) *selectionSpaceQuickLookState {
	state := &selectionSpaceQuickLookState{
		ops: make(chan func(), quickLookOpsCapacity),
		now: func() time.Time { return fake.now },
		openInstance: func() bool {
			if fake.blockOpen != nil {
				<-fake.blockOpen
			}
			fake.opened++
			return fake.openResult
		},
		closeInstance: func() { fake.closed++ },
	}
	go func() {
		defer close(fake.workerDone)
		for op := range state.ops {
			op()
		}
	}()
	return state
}

// flushQuickLookOps enqueues a barrier and waits until the worker has finished
// every earlier operation.
func flushQuickLookOps(t *testing.T, state *selectionSpaceQuickLookState) {
	t.Helper()
	done := make(chan struct{})
	state.enqueue(func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("quick look ops worker stalled")
	}
}

func quickLookKeyDown(key keyboard.Key, modifiers keyboard.Modifier) keyboard.RawKeyEvent {
	return keyboard.RawKeyEvent{Type: keyboard.EventTypeKeyDown, Key: key, Modifiers: modifiers}
}

func quickLookKeyUp(key keyboard.Key) keyboard.RawKeyEvent {
	return keyboard.RawKeyEvent{Type: keyboard.EventTypeKeyUp, Key: key}
}

func TestSpaceQuickLookShortPressOpensAndKeepsPreview(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	if !state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0)) {
		t.Fatal("clean Space keydown should be consumed")
	}
	flushQuickLookOps(t, state)
	fake.advance(50 * time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	if fake.opened != 1 || fake.closed != 0 {
		t.Fatalf("short press: opened=%d closed=%d, want one open and no close", fake.opened, fake.closed)
	}
}

func TestSpaceQuickLookSecondShortPressClosesPreview(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)
	fake.advance(50 * time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)
	fake.advance(50 * time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	if fake.opened != 1 || fake.closed != 1 {
		t.Fatalf("second short press: opened=%d closed=%d, want the first open then one close", fake.opened, fake.closed)
	}
}

func TestSpaceQuickLookLongPressReleaseClosesPreview(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)
	fake.advance(spaceQuickLookHoldDelay + 50*time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	if fake.opened != 1 || fake.closed != 1 {
		t.Fatalf("long press: opened=%d closed=%d, want peek open then close on release", fake.opened, fake.closed)
	}
}

func TestSpaceQuickLookAutoRepeatKeepsSingleOpen(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	for repeat := 0; repeat < 3; repeat++ {
		if !state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0)) {
			t.Fatal("auto-repeat keydowns must stay consumed while the press is held")
		}
	}
	fake.advance(50 * time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	if fake.opened != 1 || fake.closed != 0 {
		t.Fatalf("auto-repeat: opened=%d closed=%d, want exactly one open", fake.opened, fake.closed)
	}
}

func TestSpaceQuickLookSuppressesModifiedAndRecentTyping(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	if state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, keyboard.ModifierCtrl)) {
		t.Fatal("modified Space must pass through to Explorer")
	}
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))

	state.handleRawKey(quickLookKeyDown(keyboard.KeyX, 0))
	if state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0)) {
		t.Fatal("Space within the typing suppression window must pass through")
	}
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	if fake.opened != 0 {
		t.Fatalf("suppressed presses opened=%d, want no preview", fake.opened)
	}
}

// TestSpaceQuickLookShortKeyupBeforeOpenCompletes covers the race where the
// quick short keyup arrives while the open (selection read) is still running:
// the preview must survive the release.
func TestSpaceQuickLookShortKeyupBeforeOpenCompletes(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	fake.blockOpen = make(chan struct{})
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	fake.advance(50 * time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	close(fake.blockOpen)
	flushQuickLookOps(t, state)

	if fake.opened != 1 || fake.closed != 0 {
		t.Fatalf("short keyup during open: opened=%d closed=%d, want the preview kept", fake.opened, fake.closed)
	}
}

// TestSpaceQuickLookLongKeyupBeforeOpenCompletes covers peek release while the
// open is still in flight: the close must run after the open finishes.
func TestSpaceQuickLookLongKeyupBeforeOpenCompletes(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	fake.blockOpen = make(chan struct{})
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	fake.advance(spaceQuickLookHoldDelay + 50*time.Millisecond)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	close(fake.blockOpen)
	flushQuickLookOps(t, state)

	if fake.opened != 1 || fake.closed != 1 {
		t.Fatalf("long keyup during open: opened=%d closed=%d, want close after open completes", fake.opened, fake.closed)
	}
}

func TestSpaceQuickLookEscapeClosesOpenPreviewOnly(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)

	if !state.handleRawKey(quickLookKeyDown(keyboard.KeyEscape, 0)) {
		t.Fatal("Escape over an open preview should be consumed")
	}
	flushQuickLookOps(t, state)
	if fake.closed != 1 {
		t.Fatalf("Escape: closed=%d, want the preview closed", fake.closed)
	}

	if state.handleRawKey(quickLookKeyDown(keyboard.KeyEscape, 0)) {
		t.Fatal("Escape without an open preview must pass through")
	}
}

// TestSpaceQuickLookFailedOpenKeepsStateClosed verifies that an open which
// never dispatched a window (no valid selection) leaves the state machine
// ready for the next press instead of stuck toggling.
func TestSpaceQuickLookFailedOpenKeepsStateClosed(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	fake.openResult = false
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)
	state.handleRawKey(quickLookKeyUp(keyboard.KeySpace))
	flushQuickLookOps(t, state)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)

	if fake.opened != 2 {
		t.Fatalf("failed opens: attempts=%d, want the second press to retry opening", fake.opened)
	}
}

// TestSpaceQuickLookTeardownClosesOpenPreview verifies that disabling the
// listener while a preview is open dismisses it before the worker exits.
func TestSpaceQuickLookTeardownClosesOpenPreview(t *testing.T) {
	fake := newQuickLookFake(time.Unix(0, 0))
	state := newQuickLookTestState(fake)

	state.handleRawKey(quickLookKeyDown(keyboard.KeySpace, 0))
	flushQuickLookOps(t, state)

	state.close()
	select {
	case <-fake.workerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("quick look ops worker did not exit")
	}
	if fake.closed != 1 {
		t.Fatalf("teardown: closed=%d, want the open preview dismissed", fake.closed)
	}
}
