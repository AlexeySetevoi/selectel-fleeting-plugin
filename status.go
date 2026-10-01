package selectel

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/selectel-fleeting-plugin/internal/selectelapi"
)

// MapStatus переводит статус сервера Nova в состояние fleeting.
// Неизвестный статус — ok=false: состояние не угадываем, вызывающий пропускает.
func MapStatus(status string) (state provider.State, ok bool) {
	switch status {
	case selectelapi.StatusActive, selectelapi.StatusPassword, selectelapi.StatusMigrating,
		selectelapi.StatusResize, selectelapi.StatusVerifyResize, selectelapi.StatusRevertResize:
		return provider.StateRunning, true
	case selectelapi.StatusBuild, selectelapi.StatusReboot, selectelapi.StatusHardReboot, selectelapi.StatusRebuild:
		return provider.StateCreating, true
	case selectelapi.StatusDeleted, selectelapi.StatusSoftDeleted:
		return provider.StateDeleting, true
	case selectelapi.StatusShutoff, selectelapi.StatusExpired, selectelapi.StatusError,
		selectelapi.StatusShelved, selectelapi.StatusShelvedOff, selectelapi.StatusSuspended,
		selectelapi.StatusPaused, selectelapi.StatusRescue:
		// Плагин серверы не останавливает, только удаляет. EXPIRED — вытесненный
		// прерываемый сервер, SHUTOFF — выключенный изнутри или руками: сами они
		// не вернутся, отдаём timeout, чтобы taskscaler их удалил.
		return provider.StateTimeout, true
	default:
		// UNKNOWN — Nova потеряла связь с гипервизором, может вернуться
		return "", false
	}
}
