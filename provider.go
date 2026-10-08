package selectel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	mrand "math/rand/v2"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/sync/errgroup"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/selectel-fleeting-plugin/internal/selectelapi"
)

const (
	// сколько запросов на создание шлём одновременно
	createConcurrency = 5

	// на сколько размещение уходит в конец очереди после того, как в нём не
	// хватило ресурсов или зона не ответила
	placementCooldown = 10 * time.Minute

	// как часто Update ищет брошенные порты и адреса
	orphanCheckInterval = 5 * time.Minute
)

// подменяется в тестах
var newCloud = selectelapi.NewOpenStack

// Порядок, в котором пробуются размещения для нового сервера.
const (
	// ordered — всегда с первого из конфига, остальные только как фолбэк
	strategyOrdered = "ordered"
	// round_robin — каждый новый сервер начинает со следующего размещения
	strategyRoundRobin = "round_robin"
	// random — со случайного
	strategyRandom = "random"
)

// Placement — один вариант размещения сервера, см. InstanceGroup.Placements.
type Placement struct {
	AvailabilityZone string `json:"availability_zone"`
	Flavor           string `json:"flavor"`
}

type InstanceGroup struct {
	// Name — префикс имён серверов и значение metadata fleeting-group, по
	// которому плагин узнаёт свои машины в проекте.
	Name string `json:"name"`

	// Сервисный пользователь; всё можно задать и переменными OS_* из
	// RC-файла Selectel, они важнее конфига.
	AuthURL      string `json:"auth_url"`
	AccountID    string `json:"account_id"`
	ProjectID    string `json:"project_id"`
	Username     string `json:"username"`
	PasswordFile string `json:"password_file"`
	Region       string `json:"region"`

	AvailabilityZone string `json:"availability_zone"`
	Flavor           string `json:"flavor"`

	// Placements — упорядоченный список вариантов размещения: если в зоне
	// или под флейвор не хватило ресурсов, пробуем следующий. Взаимоисключающе
	// с AvailabilityZone/Flavor.
	Placements []Placement `json:"placements"`

	// PlacementStrategy — с какого размещения начинать: ordered (по
	// умолчанию), round_robin или random. Фолбэк при отказе идёт по всем
	// остальным размещениям по кругу.
	PlacementStrategy string `json:"placement_strategy"`

	// Cores и MemoryGB — произвольная конфигурация вместо Flavor: плагин
	// заводит под неё приватный флейвор.
	Cores    int     `json:"cores"`
	MemoryGB float64 `json:"memory_gb"`

	// ImageID и ImageName взаимоисключающие. По имени берётся самый свежий
	// образ при каждом создании, так что новые сборки подхватываются без
	// правки конфига.
	ImageID   string `json:"image_id"`
	ImageName string `json:"image_name"`

	VolumeType string `json:"volume_type"`
	DiskSizeGB int    `json:"disk_size_gb"`

	NetworkID        string   `json:"network_id"`
	SubnetID         string   `json:"subnet_id"`
	SecurityGroupIDs []string `json:"security_group_ids"`

	// FloatingIP выдаёт серверу публичный адрес. Адрес создаётся вместе с
	// сервером и удаляется вместе с ним.
	FloatingIP        bool   `json:"floating_ip"`
	FloatingNetworkID string `json:"floating_network_id"`

	Preemptible bool `json:"preemptible"`

	Tags     []string          `json:"tags"`
	Metadata map[string]string `json:"metadata"`

	// UserData и UserDataFile взаимоисключающие: cloud-init для Linux,
	// cloudbase-init для Windows.
	UserData     string `json:"user_data"`
	UserDataFile string `json:"user_data_file"`

	log      hclog.Logger
	settings provider.Settings
	client   selectelapi.Cloud
	password string

	placements []Placement
	// ID флейвора по имени из конфига; "" — произвольная конфигурация
	flavorIDs map[string]string

	publicKey string
	keyName   string

	// За принятыми запросами на создание следят фоновые горутины: Increase
	// их не ждёт, иначе встаёт весь цикл раннера (он однопоточный).
	watchCtx    context.Context
	watchCancel context.CancelFunc
	watchers    sync.WaitGroup

	now             func() time.Time
	mu              sync.Mutex
	exhaustedUntil  map[Placement]time.Time
	nextOrphanCheck time.Time
	// следующее стартовое размещение для round_robin
	nextPlacement int
}

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

func (g *InstanceGroup) Init(ctx context.Context, log hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.settings = settings
	g.applyEnv()
	g.log = log.With("project_id", g.ProjectID, "region", g.Region, "name", g.Name)

	if err := g.validate(); err != nil {
		return provider.ProviderInfo{}, err
	}
	if err := g.populate(); err != nil {
		return provider.ProviderInfo{}, err
	}

	client, err := newCloud(ctx, selectelapi.Auth{
		AuthURL:   g.AuthURL,
		AccountID: g.AccountID,
		Username:  g.Username,
		Password:  g.password,
		ProjectID: g.ProjectID,
		Region:    g.Region,
	})
	if err != nil {
		return provider.ProviderInfo{}, err
	}
	g.client = client

	if err := g.resolveFlavors(ctx); err != nil {
		return provider.ProviderInfo{}, err
	}

	if g.FloatingIP && g.FloatingNetworkID == "" {
		if g.FloatingNetworkID, err = g.client.ExternalNetworkID(ctx); err != nil {
			return provider.ProviderInfo{}, err
		}
	}

	if err := g.setupSSHKey(); err != nil {
		return provider.ProviderInfo{}, err
	}
	if g.publicKey != "" {
		// своя keypair на каждый запуск: при перезапуске раннера старый процесс
		// удаляет свою, не задевая новую
		name := "fleeting-" + g.Name + "-" + randomSuffix()
		if err := g.client.CreateKeypair(ctx, name, g.publicKey); err != nil {
			return provider.ProviderInfo{}, err
		}
		g.keyName = name
	}

	g.watchCtx, g.watchCancel = context.WithCancel(context.Background())
	g.exhaustedUntil = map[Placement]time.Time{}
	if g.now == nil {
		g.now = time.Now
	}

	return provider.ProviderInfo{
		ID:        path.Join("selectel", g.ProjectID, g.Region, g.Name),
		MaxSize:   math.MaxInt,
		Version:   Version.String(),
		BuildInfo: Version.BuildInfo(),
	}, nil
}

// resolveFlavors переводит имена флейворов в ID один раз при старте:
// опечатка в имени видна сразу, а не при первом создании.
func (g *InstanceGroup) resolveFlavors(ctx context.Context) error {
	g.flavorIDs = map[string]string{}

	for _, p := range g.placements {
		if _, ok := g.flavorIDs[p.Flavor]; ok {
			continue
		}

		var (
			id  string
			err error
		)
		if p.Flavor == "" {
			ramMB := int(g.MemoryGB * 1024)
			id, err = g.client.CustomFlavor(ctx, g.Cores, ramMB)
			if err != nil {
				return fmt.Errorf("could not get flavor for %d vcpu / %d MB: %w", g.Cores, ramMB, err)
			}
			g.log.Info("using custom flavor", "flavor_id", id, "cores", g.Cores, "ram_mb", ramMB)
		} else if id, err = g.client.FlavorID(ctx, p.Flavor); err != nil {
			return fmt.Errorf("could not resolve flavor: %w", err)
		}

		g.flavorIDs[p.Flavor] = id
	}

	return nil
}

func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	instances, err := g.client.ListInstances(ctx, g.Name+"-")
	if err != nil {
		return fmt.Errorf("could not list instances: %w", err)
	}

	for _, instance := range instances {
		if instance.Metadata[metadataGroup] != g.Name {
			continue
		}

		state, ok := MapStatus(instance.Status)
		if !ok {
			g.log.Warn("unrecognized instance status, skipping", "id", instance.ID, "status", instance.Status)
			continue
		}
		g.log.Debug("instance status", "id", instance.ID, "status", instance.Status, "state", state)

		update(instance.ID, state)
	}

	return g.cleanupOrphans(ctx)
}

// cleanupOrphans убирает порты и адреса группы, оставшиеся без сервера:
// плагин упал между созданием порта и сервера или сервер удалили руками.
// Не чаще раза в orphanCheckInterval.
func (g *InstanceGroup) cleanupOrphans(ctx context.Context) error {
	now := g.now()
	if now.Before(g.nextOrphanCheck) {
		return nil
	}
	g.nextOrphanCheck = now.Add(orphanCheckInterval)

	deleted, err := g.client.CleanupOrphans(ctx, g.Name, now.Add(-selectelapi.OrphanAge))
	if deleted > 0 {
		g.log.Info("deleted orphaned ports and floating ips", "count", deleted)
	}
	if err != nil {
		return fmt.Errorf("could not clean up orphaned ports and floating ips: %w", err)
	}
	return nil
}

// Increase возвращается, как только Nova приняла запросы: сервер с этого
// момента виден в Update как creating. Ошибки квоты, прав и конфигурации
// приходят сразу и попадают в ответ; то, что выясняется позже (нехватка
// ресурсов в зоне), ловит watch.
func (g *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	var (
		mu        sync.Mutex
		errs      []error
		succeeded int
	)

	var eg errgroup.Group
	eg.SetLimit(createConcurrency)

	for range delta {
		eg.Go(func() error {
			op, placement, err := g.createInstance(ctx)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
				return nil
			}

			g.log.Info("instance requested", "id", op.InstanceID(), "availability_zone", placement.AvailabilityZone, "flavor", placement.Flavor)
			g.watch(op, placement)
			succeeded++
			return nil
		})
	}
	_ = eg.Wait()

	return succeeded, errors.Join(errs...)
}

// createInstance перебирает варианты размещения по порядку; к следующему
// переходит только когда не хватило ресурсов или квоты, любая другая ошибка
// от смены зоны не вылечится.
func (g *InstanceGroup) createInstance(ctx context.Context) (selectelapi.CreateOperation, Placement, error) {
	imageID := g.ImageID
	if g.ImageName != "" {
		var err error
		if imageID, err = g.client.LatestImageByName(ctx, g.ImageName); err != nil {
			return nil, Placement{}, fmt.Errorf("could not resolve image %q: %w", g.ImageName, err)
		}
	}

	var errs []error

	candidates := g.orderedPlacements()
	for i, p := range candidates {
		op, err := g.client.CreateInstance(ctx, selectelapi.CreateInstanceRequest{
			// имя новое на каждую попытку, чтобы порт и сервер не путались
			Name:              g.Name + "-" + randomSuffix(),
			Metadata:          g.instanceMetadata(),
			Tags:              g.instanceTags(),
			AvailabilityZone:  p.AvailabilityZone,
			FlavorID:          g.flavorIDs[p.Flavor],
			ImageID:           imageID,
			VolumeType:        volumeType(g.VolumeType, p.AvailabilityZone),
			DiskSizeGB:        g.DiskSizeGB,
			NetworkID:         g.NetworkID,
			SubnetID:          g.SubnetID,
			SecurityGroupIDs:  g.SecurityGroupIDs,
			FloatingNetworkID: g.FloatingNetworkID,
			KeyName:           g.keyName,
			UserData:          g.UserData,
			Group:             g.Name,
		})
		if err == nil {
			return op, p, nil
		}

		errs = append(errs, fmt.Errorf("zone %s flavor %s: %w", p.AvailabilityZone, p.Flavor, err))

		if i < len(candidates)-1 && placementFailure(err) {
			g.log.Warn("placement failed, trying next placement", "availability_zone", p.AvailabilityZone, "flavor", p.Flavor, "error", err)
			continue
		}

		break
	}

	return nil, Placement{}, fmt.Errorf("could not create instance: %w", errors.Join(errs...))
}

// placementFailure — ошибки, которые лечатся сменой зоны или флейвора.
func placementFailure(err error) bool {
	return errors.Is(err, selectelapi.ErrResourceExhausted) || errors.Is(err, selectelapi.ErrUnavailable)
}

// orderedPlacements — размещения по кругу от стартового (см. PlacementStrategy),
// но те, что недавно отказали, идут последними: их пробуем, только если
// остальные отказали.
func (g *InstanceGroup) orderedPlacements() []Placement {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	n := len(g.placements)

	start := 0
	switch g.PlacementStrategy {
	case strategyRoundRobin:
		start = g.nextPlacement % n
		g.nextPlacement = (start + 1) % n
	case strategyRandom:
		start = mrand.IntN(n)
	}

	ordered := make([]Placement, 0, n)
	var exhausted []Placement
	for i := range n {
		p := g.placements[(start+i)%n]
		if until, ok := g.exhaustedUntil[p]; ok && now.Before(until) {
			exhausted = append(exhausted, p)
			continue
		}
		delete(g.exhaustedUntil, p)
		ordered = append(ordered, p)
	}

	return append(ordered, exhausted...)
}

// watch дожидается ACTIVE в фоне. Сервер, упавший в ERROR, остаётся в
// проекте: Update отдаёт его как timeout, и раннер удаляет его и запрашивает
// замену — к тому моменту размещение уже в конце очереди.
func (g *InstanceGroup) watch(op selectelapi.CreateOperation, p Placement) {
	g.watchers.Add(1)

	go func() {
		defer g.watchers.Done()

		instance, err := op.Wait(g.watchCtx)
		if err == nil {
			g.log.Info("instance created", "id", instance.ID, "instance_name", instance.Name)
			return
		}
		if g.watchCtx.Err() != nil {
			return // плагин останавливается, сервер здесь ни при чём
		}

		g.log.Error("instance creation failed after the request was accepted", "id", op.InstanceID(), "availability_zone", p.AvailabilityZone, "flavor", p.Flavor, "error", err)

		if placementFailure(err) {
			g.mu.Lock()
			g.exhaustedUntil[p] = g.now().Add(placementCooldown)
			g.mu.Unlock()

			g.log.Warn("placement moved to the end of the list", "availability_zone", p.AvailabilityZone, "flavor", p.Flavor, "for", placementCooldown)
		}
	}()
}

func (g *InstanceGroup) instanceMetadata() map[string]string {
	metadata := maps.Clone(g.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	metadata[metadataGroup] = g.Name
	metadata[metadataManagedBy] = NAME
	return metadata
}

func (g *InstanceGroup) instanceTags() []string {
	tags := slices.Clone(g.Tags)
	if g.Preemptible {
		tags = append(tags, tagPreemptible)
	}
	return tags
}

func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	if len(instances) == 0 {
		return nil, nil
	}

	var errs []error
	deleted := make([]string, 0, len(instances))

	for _, id := range instances {
		if err := g.client.DeleteInstance(ctx, id, g.Name); err != nil {
			if errors.Is(err, selectelapi.ErrNotFound) {
				g.log.Warn("tried to delete an instance that does not exist", "id", id)
				deleted = append(deleted, id)
				continue
			}

			errs = append(errs, fmt.Errorf("could not delete instance %s: %w", id, err))
			continue
		}

		deleted = append(deleted, id)
	}

	return deleted, errors.Join(errs...)
}

func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	info := provider.ConnectInfo{ConnectorConfig: g.settings.ConnectorConfig}

	instance, err := g.client.GetInstance(ctx, id)
	if err != nil {
		return info, fmt.Errorf("could not get instance: %w", err)
	}

	info.ID = id
	info.InternalAddr = instance.InternalIP
	info.ExternalAddr = instance.ExternalIP

	return info, nil
}

func (g *InstanceGroup) Heartbeat(_ context.Context, _ string) error {
	return nil
}

func (g *InstanceGroup) Suspend(_ context.Context, _ []string) ([]string, error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

func (g *InstanceGroup) Resume(_ context.Context, _ []string) ([]string, error) {
	return nil, provider.ErrSuspendResumeNotSupported
}

func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	if g.watchCancel != nil {
		g.watchCancel()
		g.watchers.Wait()
	}

	if g.client == nil || g.keyName == "" {
		return nil
	}
	// ключ уже лежит в созданных серверах, keypair для них не нужна
	return g.client.DeleteKeypair(ctx, g.keyName)
}

func randomSuffix() string {
	b := make([]byte, 4)
	// crypto/rand.Read с Go 1.24 не возвращает ошибку
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
