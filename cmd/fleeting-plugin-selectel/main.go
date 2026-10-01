package main

import (
	"os/signal"
	"syscall"

	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	selectel "github.com/AlexeySetevoi/selectel-fleeting-plugin"
)

func main() {
	// Сигналы остановки (SIGTERM от systemd и docker stop, SIGQUIT — мягкая
	// остановка раннера, SIGHUP — перечитывание конфига) приходят всей группе
	// процессов, а Go по умолчанию на них выходит. Плагин умирал раньше, чем
	// раннер вызовет Shutdown, и keypair запуска оставалась в проекте. Жизнью
	// плагина управляет раннер (Shutdown, затем Kill), поэтому сигналы
	// игнорируем; SIGINT go-plugin игнорирует сам.
	signal.Ignore(syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	plugin.Main(&selectel.InstanceGroup{}, selectel.Version)
}
