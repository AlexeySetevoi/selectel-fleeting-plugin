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
		"SHUTOFF":      provider.StateDeleting,
		"EXPIRED":      provider.StateDeleting,
		"ERROR":        provider.StateDeleting,
		"SHELVED":      provider.StateDeleting,
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

// Статус, добавленный в список, должен попасть и в MapStatus. UNKNOWN не
// сопоставлен намеренно, Update держит его как creating.
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

// timeout taskscaler забывает без Decrease — его не отдаём ни для какого статуса.
func TestMapStatusNeverTimeout(t *testing.T) {
	for _, status := range selectelapi.AllStatuses {
		if state, _ := MapStatus(status); state == provider.StateTimeout {
			t.Errorf("MapStatus(%s) = timeout", status)
		}
	}
}

func TestBroken(t *testing.T) {
	for _, status := range []string{"ERROR", "SHUTOFF", "EXPIRED", "SHELVED", "SHELVED_OFFLOADED", "SUSPENDED", "PAUSED", "RESCUE"} {
		if !Broken(status) {
			t.Errorf("Broken(%s) = false", status)
		}
	}
	for _, status := range []string{"ACTIVE", "BUILD", "REBOOT", "DELETED", "UNKNOWN"} {
		if Broken(status) {
			t.Errorf("Broken(%s) = true", status)
		}
	}
}
