package selectel

import (
	"testing"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/selectel-fleeting-plugin/internal/selectelapi"
)

func TestMapStatus(t *testing.T) {
	tests := map[string]provider.State{
		"BUILD":        provider.StateCreating,
		"REBOOT":       provider.StateCreating,
		"HARD_REBOOT":  provider.StateCreating,
		"REBUILD":      provider.StateCreating,
		"ACTIVE":       provider.StateRunning,
		"PASSWORD":     provider.StateRunning,
		"MIGRATING":    provider.StateRunning,
		"DELETED":      provider.StateDeleting,
		"SOFT_DELETED": provider.StateDeleting,
		"SHUTOFF":      provider.StateTimeout,
		"EXPIRED":      provider.StateTimeout,
		"ERROR":        provider.StateTimeout,
		"SHELVED":      provider.StateTimeout,
	}

	for status, want := range tests {
		got, ok := MapStatus(status)
		if !ok || got != want {
			t.Errorf("MapStatus(%q) = %q, %v, want %q", status, got, ok, want)
		}
	}

	for _, status := range []string{"UNKNOWN", "SOMETHING_NEW"} {
		if _, ok := MapStatus(status); ok {
			t.Errorf("MapStatus(%s) reported ok", status)
		}
	}
}

// Статус, добавленный в список, должен попасть и в MapStatus, а не молча
// пропускаться в Update. UNKNOWN пропускается намеренно.
func TestMapStatusCoversAllStatuses(t *testing.T) {
	for _, status := range selectelapi.AllStatuses {
		if status == selectelapi.StatusUnknown {
			continue
		}
		if _, ok := MapStatus(status); !ok {
			t.Errorf("server status %s is not mapped", status)
		}
	}
}
