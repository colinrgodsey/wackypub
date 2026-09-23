package agent

import "testing"

// TestToolEventIsAnnounce pins the announce/update discriminator that the bridge layer
// depends on: live announce (status empty) and denied announce (status denied + flag) are
// tool_call; completed/error updates are tool_call_update.
func TestToolEventIsAnnounce(t *testing.T) {
	cases := []struct {
		name string
		ev   ToolEvent
		want bool
	}{
		{"live announce", ToolEvent{Status: ""}, true},
		{"denied announce", ToolEvent{Status: "denied", Denied: true}, true},
		{"completed update", ToolEvent{Status: "completed"}, false},
		{"error update", ToolEvent{Status: "error"}, false},
		{"denied update half", ToolEvent{Status: "denied"}, false},
	}
	for _, tc := range cases {
		if got := toolEventIsAnnounce(tc.ev); got != tc.want {
			t.Errorf("%s: toolEventIsAnnounce(%+v) = %v, want %v", tc.name, tc.ev, got, tc.want)
		}
	}
}
