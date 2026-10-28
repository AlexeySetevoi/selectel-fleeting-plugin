package selectel

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/selectel-fleeting-plugin/internal/selectelapi"
)

// MapStatus переводит статус сервера Nova в состояние fleeting.
// Неизвестный статус — ok=false: состояние не угадываем, решает вызывающий.
//
// StateTimeout не отдаём никогда: для taskscaler это «инстанса больше нет», он
// забывает его без Decrease, и сервер остаётся в проекте навсегда.
func MapStatus(status string) (state provider.State, ok bool) {
	switch status {
	case selectelapi.StatusActive, selectelapi.StatusPassword, selectelapi.StatusMigrating,
		selectelapi.StatusResize, selectelapi.StatusVerifyResize, selectelapi.StatusRevertResize:
		return provider.StateRunning, true
	case selectelapi.StatusBuild, selectelapi.StatusReboot, selectelapi.StatusHardReboot, selectelapi.StatusRebuild:
		return provider.StateCreating, true
	case selectelapi.StatusDeleted, selectelapi.StatusSoftDeleted:
		return provider.StateDeleting, true
	}
	if Broken(status) {
		// Update такие серверы удаляет сам, см. Broken
		return provider.StateDeleting, true
	}
	// UNKNOWN — Nova потеряла связь с гипервизором, может вернуться
	return "", false
}

// Broken — сервер сам не вернётся, и удалить его должен плагин. Плагин серверы
// не останавливает, только удаляет: EXPIRED — вытесненный прерываемый сервер,
// SHUTOFF — выключенный изнутри или руками, ERROR — не создался.
func Broken(status string) bool {
	switch status {
	case selectelapi.StatusShutoff, selectelapi.StatusExpired, selectelapi.StatusError,
		selectelapi.StatusShelved, selectelapi.StatusShelvedOff, selectelapi.StatusSuspended,
		selectelapi.StatusPaused, selectelapi.StatusRescue:
		return true
	}
	return false
}
