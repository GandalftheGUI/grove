package daemon

import (
	"testing"
	"time"

	"github.com/ianremillard/grove/internal/proto"
	"github.com/stretchr/testify/assert"
)


func TestInfoWaitingPromotion(t *testing.T) {
	inst := &Instance{
		ID:             "1",
		Project:        "my-app",
		Branch:         "main",
		CreatedAt:      time.Now().Add(-10 * time.Minute),
		state:          proto.StateRunning,
		lastOutputTime: time.Now().Add(-3 * time.Second), // idle >2s
	}

	info := inst.Info()
	assert.Equal(t, proto.StateWaiting, info.State)
}

func TestInfoRunningWhenRecentOutput(t *testing.T) {
	inst := &Instance{
		ID:             "1",
		Project:        "my-app",
		Branch:         "main",
		CreatedAt:      time.Now().Add(-1 * time.Minute),
		state:          proto.StateRunning,
		lastOutputTime: time.Now(), // just produced output
	}

	info := inst.Info()
	assert.Equal(t, proto.StateRunning, info.State)
}

func TestInfoNonRunningStateUnchanged(t *testing.T) {
	for _, state := range []string{
		proto.StateExited, proto.StateCrashed, proto.StateKilled, proto.StateFinished,
	} {
		inst := &Instance{
			ID:             "1",
			state:          state,
			lastOutputTime: time.Now().Add(-10 * time.Second),
		}
		assert.Equal(t, state, inst.Info().State, "state %s should not be promoted", state)
	}
}
