package cmd

import (
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func TestClassifyLock(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	written := func(age time.Duration) adkAgent.AgentLockObservation {
		return adkAgent.AgentLockObservation{SessionExists: true, LastWrite: now.Add(-age)}
	}
	held := func(age time.Duration) adkAgent.AgentLockObservation {
		obs := written(age)
		obs.LockExists = true
		obs.HolderPID = 4242
		obs.HolderPIDValid = true
		obs.HolderAlive = true
		return obs
	}

	tests := []struct {
		name        string
		obs         adkAgent.AgentLockObservation
		wantStatus  string
		wantVerdict string
	}{
		{name: "no lock file and recent write is free and ok", obs: written(2 * time.Minute), wantStatus: "FREE", wantVerdict: "OK"},
		{name: "no lock file and an hour of silence is idle", obs: written(time.Hour), wantStatus: "FREE", wantVerdict: "IDLE"},
		{name: "no lock file and no session file is idle", obs: adkAgent.AgentLockObservation{}, wantStatus: "FREE", wantVerdict: "IDLE"},
		{name: "live holder writing now is ok", obs: held(2 * time.Second), wantStatus: "HELD", wantVerdict: "OK"},
		{name: "live holder inside the wedge grace window stays ok", obs: held(7 * time.Minute), wantStatus: "HELD", wantVerdict: "OK"},
		{name: "live holder quiet past the wedge threshold is wedged", obs: held(11 * time.Minute), wantStatus: "HELD", wantVerdict: "WEDGED"},
		{
			name:        "live holder that never wrote a session is ok",
			obs:         adkAgent.AgentLockObservation{LockExists: true, HolderPID: 4242, HolderPIDValid: true, HolderAlive: true},
			wantStatus:  "HELD",
			wantVerdict: "OK",
		},
		{
			name:        "dead holder with an old session is stale",
			obs:         adkAgent.AgentLockObservation{LockExists: true, HolderPID: 4242, HolderPIDValid: true, SessionExists: true, LastWrite: now.Add(-time.Hour)},
			wantStatus:  "STALE",
			wantVerdict: "STALE",
		},
		{
			name:        "dead holder that wrote seconds ago just exited normally",
			obs:         adkAgent.AgentLockObservation{LockExists: true, HolderPID: 4242, HolderPIDValid: true, SessionExists: true, LastWrite: now.Add(-30 * time.Second)},
			wantStatus:  "STALE",
			wantVerdict: "OK",
		},
		{
			name:        "lock file without a readable pid is unknown",
			obs:         adkAgent.AgentLockObservation{LockExists: true},
			wantStatus:  "UNKNOWN",
			wantVerdict: "IDLE",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, verdict := classifyLock(tc.obs, now)
			if status != tc.wantStatus || verdict != tc.wantVerdict {
				t.Fatalf("classifyLock() = (%s, %s), want (%s, %s)", status, verdict, tc.wantStatus, tc.wantVerdict)
			}
		})
	}
}

func TestLocksAge(t *testing.T) {
	tests := []struct {
		age  time.Duration
		want string
	}{
		{45 * time.Second, "45s ago"},
		{9*time.Minute + 30*time.Second, "9m ago"},
		{3 * time.Hour, "3h ago"},
		{50 * time.Hour, "2d ago"},
	}
	for _, tc := range tests {
		if got := locksAge(tc.age); got != tc.want {
			t.Errorf("locksAge(%v) = %q, want %q", tc.age, got, tc.want)
		}
	}
}
