// Package selectelapi — узкая прослойка над OpenStack API облачных серверов
// Selectel (Nova, Neutron, Glance): только то, что нужно плагину, и плоские
// структуры вместо ответов gophercloud.
package selectelapi

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound — сервера/образа/флейвора нет.
	ErrNotFound = errors.New("not found")
	// ErrResourceExhausted — не хватило квоты или ресурсов пула/зоны.
	ErrResourceExhausted = errors.New("resource exhausted")
)

// Статусы сервера, как их отдаёт Nova (плюс EXPIRED — вытесненный
// прерываемый сервер Selectel).
const (
	StatusActive       = "ACTIVE"
	StatusBuild        = "BUILD"
	StatusDeleted      = "DELETED"
	StatusError        = "ERROR"
	StatusHardReboot   = "HARD_REBOOT"
	StatusMigrating    = "MIGRATING"
	StatusPassword     = "PASSWORD"
	StatusPaused       = "PAUSED"
	StatusReboot       = "REBOOT"
	StatusRebuild      = "REBUILD"
	StatusRescue       = "RESCUE"
	StatusResize       = "RESIZE"
	StatusRevertResize = "REVERT_RESIZE"
	StatusShelved      = "SHELVED"
	StatusShelvedOff   = "SHELVED_OFFLOADED"
	StatusShutoff      = "SHUTOFF"
	StatusSoftDeleted  = "SOFT_DELETED"
	StatusSuspended    = "SUSPENDED"
	StatusUnknown      = "UNKNOWN"
	StatusVerifyResize = "VERIFY_RESIZE"
	StatusExpired      = "EXPIRED"
)

// AllStatuses — все статусы выше; тест сверяет с ними таблицу MapStatus.
var AllStatuses = []string{
	StatusActive, StatusBuild, StatusDeleted, StatusError, StatusHardReboot,
	StatusMigrating, StatusPassword, StatusPaused, StatusReboot, StatusRebuild,
	StatusRescue, StatusResize, StatusRevertResize, StatusShelved, StatusShelvedOff,
	StatusShutoff, StatusSoftDeleted, StatusSuspended, StatusUnknown, StatusVerifyResize,
	StatusExpired,
}

type Instance struct {
	ID       string
	Name     string
	Status   string
	Metadata map[string]string

	// Первый IPv4 fixed-адрес и первый floating (или fixed из публичной
	// подсети, если floating нет).
	InternalIP string
	ExternalIP string

	// Fault — сообщение Nova для сервера в ERROR.
	Fault string
}

type CreateInstanceRequest struct {
	Name     string
	Metadata map[string]string
	Tags     []string

	AvailabilityZone string
	FlavorID         string

	ImageID    string
	VolumeType string
	DiskSizeGB int

	NetworkID        string
	SubnetID         string
	SecurityGroupIDs []string
	// FloatingNetworkID — внешняя сеть, из которой выдаётся floating IP;
	// пустая — без публичного адреса.
	FloatingNetworkID string

	KeyName  string
	UserData string

	// Group помечает порт и floating IP (description), чтобы найти сирот.
	Group string
}

// CreateOperation — принятый облаком запрос на создание сервера.
type CreateOperation interface {
	// InstanceID известен сразу: сервер уже виден в списке как BUILD.
	InstanceID() string
	// Wait ждёт ACTIVE. Нехватка ресурсов («No valid host») приходит именно
	// здесь, сервер при этом остаётся в ERROR.
	Wait(ctx context.Context) (*Instance, error)
}

type Cloud interface {
	// ListInstances возвращает серверы проекта с именем, начинающимся с
	// prefix (пагинация внутри).
	ListInstances(ctx context.Context, prefix string) ([]Instance, error)
	GetInstance(ctx context.Context, id string) (*Instance, error)
	// CreateInstance создаёт порт (и floating IP), затем сервер; возвращается,
	// как только Nova приняла запрос. Если сервер не принят, порт и адрес
	// удаляются.
	CreateInstance(ctx context.Context, req CreateInstanceRequest) (CreateOperation, error)
	// DeleteInstance удаляет сервер, его floating IP и порты группы; удаления
	// сервера не ждёт.
	DeleteInstance(ctx context.Context, id, group string) error
	// CleanupOrphans удаляет порты и floating IP группы, которые ни к чему не
	// привязаны и созданы раньше before. Возвращает число удалённых.
	CleanupOrphans(ctx context.Context, group string, before time.Time) (int, error)

	LatestImageByName(ctx context.Context, name string) (string, error)
	// FlavorID принимает имя или ID флейвора.
	FlavorID(ctx context.Context, nameOrID string) (string, error)
	// CustomFlavor находит приватный флейвор плагина с такими vCPU/RAM или
	// создаёт его (произвольная конфигурация, загрузка с сетевого диска).
	CustomFlavor(ctx context.Context, vcpus, ramMB int) (string, error)
	// ExternalNetworkID — единственная внешняя сеть проекта.
	ExternalNetworkID(ctx context.Context) (string, error)

	CreateKeypair(ctx context.Context, name, publicKey string) error
	DeleteKeypair(ctx context.Context, name string) error
}

// OrphanAge — сколько ждать, прежде чем считать порт или адрес без сервера
// сиротой: запас на случай, если сервер ещё создаётся.
const OrphanAge = 10 * time.Minute
