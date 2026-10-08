package selectelapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/keypairs"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/pagination"
)

const (
	// 2.72 — минимум, с которым Selectel принимает тег preemptible; теги при
	// создании и fault в ответе есть и раньше
	computeMicroversion = "2.72"

	listPageSize = 1000
	// как часто опрашиваем создаваемый сервер
	pollInterval = 2 * time.Second

	// префикс приватных флейворов, которые создаёт плагин
	customFlavorPrefix = "fleeting-"
)

// Auth — сервисный пользователь Selectel и проект, в который он входит.
type Auth struct {
	AuthURL string
	// AccountID — номер аккаунта Selectel, он же домен пользователя в Keystone.
	AccountID string
	Username  string
	Password  string
	ProjectID string
	// Region — пул (ru-9, ru-7, ...): по нему выбираются адреса сервисов из
	// каталога Keystone.
	Region string
}

type openStack struct {
	compute *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	image   *gophercloud.ServiceClient

	pageSize     int
	pollInterval time.Duration
}

func NewOpenStack(ctx context.Context, auth Auth) (Cloud, error) {
	return newOpenStack(ctx, auth)
}

func newOpenStack(ctx context.Context, auth Auth) (*openStack, error) {
	provider, err := openstack.AuthenticatedClient(ctx, gophercloud.AuthOptions{
		IdentityEndpoint: auth.AuthURL,
		Username:         auth.Username,
		Password:         auth.Password,
		DomainName:       auth.AccountID,
		// токен живёт сутки, раннер — дольше
		AllowReauth: true,
		Scope: &gophercloud.AuthScope{
			ProjectID: auth.ProjectID,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("could not authenticate in selectel keystone: %w", err)
	}

	endpoint := gophercloud.EndpointOpts{Region: auth.Region}

	compute, err := openstack.NewComputeV2(provider, endpoint)
	if err != nil {
		return nil, fmt.Errorf("could not find compute endpoint in region %s: %w", auth.Region, err)
	}
	compute.Microversion = computeMicroversion

	network, err := openstack.NewNetworkV2(provider, endpoint)
	if err != nil {
		return nil, fmt.Errorf("could not find network endpoint in region %s: %w", auth.Region, err)
	}

	image, err := openstack.NewImageV2(provider, endpoint)
	if err != nil {
		return nil, fmt.Errorf("could not find image endpoint in region %s: %w", auth.Region, err)
	}

	return &openStack{
		compute:      compute,
		network:      network,
		image:        image,
		pageSize:     listPageSize,
		pollInterval: pollInterval,
	}, nil
}

func (c *openStack) ListInstances(ctx context.Context, prefix string) ([]Instance, error) {
	var result []Instance

	// name в Nova — регулярное выражение
	opts := servers.ListOpts{Name: "^" + regexp.QuoteMeta(prefix), Limit: c.pageSize}
	err := servers.List(c.compute, opts).EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		list, err := servers.ExtractServers(page)
		if err != nil {
			return false, err
		}
		for i := range list {
			result = append(result, fromServer(&list[i]))
		}
		return true, nil
	})
	if err != nil {
		return nil, mapError(err)
	}

	return result, nil
}

func (c *openStack) GetInstance(ctx context.Context, id string) (*Instance, error) {
	server, err := servers.Get(ctx, c.compute, id).Extract()
	if err != nil {
		return nil, mapError(err)
	}

	result := fromServer(server)
	return &result, nil
}

func (c *openStack) CreateInstance(ctx context.Context, req CreateInstanceRequest) (CreateOperation, error) {
	port, fip, err := c.createPort(ctx, req)
	if err != nil {
		return nil, err
	}

	server, err := c.createServer(ctx, req, port.ID)
	if err != nil {
		// сервер не принят — порт и адрес никому не нужны
		if cleanupErr := c.deletePortAndAddress(ctx, port.ID, fip); cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("could not clean up port %s: %w", port.ID, cleanupErr))
		}
		return nil, err
	}

	return &createOperation{c: c, id: server.ID}, nil
}

// createPort заводит порт заранее, а не отдаёт это Nova: так floating IP
// привязывается сразу и адрес известен до старта сервера, а группы
// безопасности задаются по ID.
func (c *openStack) createPort(ctx context.Context, req CreateInstanceRequest) (*ports.Port, *floatingips.FloatingIP, error) {
	opts := ports.CreateOpts{
		NetworkID:   req.NetworkID,
		Name:        req.Name,
		Description: groupMarker(req.Group),
	}
	if req.SubnetID != "" {
		opts.FixedIPs = []ports.IP{{SubnetID: req.SubnetID}}
	}
	if len(req.SecurityGroupIDs) > 0 {
		opts.SecurityGroups = &req.SecurityGroupIDs
	}

	port, err := ports.Create(ctx, c.network, opts).Extract()
	if err != nil {
		return nil, nil, fmt.Errorf("could not create port: %w", mapError(err))
	}

	if req.FloatingNetworkID == "" {
		return port, nil, nil
	}

	fip, err := floatingips.Create(ctx, c.network, floatingips.CreateOpts{
		FloatingNetworkID: req.FloatingNetworkID,
		PortID:            port.ID,
		Description:       groupMarker(req.Group),
	}).Extract()
	if err != nil {
		err = fmt.Errorf("could not create floating ip: %w", mapError(err))
		if cleanupErr := c.deletePortAndAddress(ctx, port.ID, nil); cleanupErr != nil {
			return nil, nil, errors.Join(err, fmt.Errorf("could not clean up port %s: %w", port.ID, cleanupErr))
		}
		return nil, nil, err
	}

	return port, fip, nil
}

func (c *openStack) createServer(ctx context.Context, req CreateInstanceRequest, portID string) (*servers.Server, error) {
	opts := servers.CreateOpts{
		Name:             req.Name,
		FlavorRef:        req.FlavorID,
		AvailabilityZone: req.AvailabilityZone,
		Metadata:         req.Metadata,
		Tags:             req.Tags,
		Networks:         []servers.Network{{Port: portID}},
		BlockDevice: []servers.BlockDevice{{
			SourceType:          servers.SourceImage,
			UUID:                req.ImageID,
			DestinationType:     servers.DestinationVolume,
			VolumeSize:          req.DiskSizeGB,
			VolumeType:          req.VolumeType,
			BootIndex:           0,
			DeleteOnTermination: true,
		}},
	}
	if req.UserData != "" {
		// gophercloud сам кодирует в base64
		opts.UserData = []byte(req.UserData)
	}

	var builder servers.CreateOptsBuilder = opts
	if req.KeyName != "" {
		builder = keypairs.CreateOptsExt{CreateOptsBuilder: opts, KeyName: req.KeyName}
	}

	server, err := servers.Create(ctx, c.compute, builder, nil).Extract()
	if err != nil {
		return nil, fmt.Errorf("could not create server: %w", mapError(err))
	}
	return server, nil
}

type createOperation struct {
	c  *openStack
	id string
}

func (o *createOperation) InstanceID() string {
	return o.id
}

func (o *createOperation) Wait(ctx context.Context) (*Instance, error) {
	ticker := time.NewTicker(o.c.pollInterval)
	defer ticker.Stop()

	for {
		instance, err := o.c.GetInstance(ctx, o.id)
		if err != nil {
			return nil, err
		}

		switch instance.Status {
		case StatusActive:
			return instance, nil
		case StatusError:
			return nil, faultError(instance.Fault)
		case StatusBuild:
		default:
			return nil, fmt.Errorf("server left BUILD with status %s", instance.Status)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// faultError: «No valid host» — планировщику не нашлось места в зоне под
// этот флейвор, то есть не хватило ресурсов.
func faultError(fault string) error {
	err := fmt.Errorf("server went to ERROR: %s", fault)
	if strings.Contains(fault, "No valid host") {
		return fmt.Errorf("%w: %w", ErrResourceExhausted, err)
	}
	return err
}

func (c *openStack) DeleteInstance(ctx context.Context, id, group string) error {
	// порты ищем до удаления: потом Nova отвяжет их от сервера
	owned, err := c.listGroupPorts(ctx, group, id)
	if err != nil {
		return err
	}

	var errs []error

	if err := servers.Delete(ctx, c.compute, id).ExtractErr(); err != nil {
		errs = append(errs, mapError(err))
	}

	for _, port := range owned {
		if err := c.deletePortAndAddress(ctx, port.ID, nil); err != nil {
			errs = append(errs, fmt.Errorf("could not delete port %s: %w", port.ID, err))
		}
	}

	return errors.Join(errs...)
}

func (c *openStack) CleanupOrphans(ctx context.Context, group string, before time.Time) (int, error) {
	var (
		errs    []error
		deleted int
	)

	groupPorts, err := c.listGroupPorts(ctx, group, "")
	if err != nil {
		return 0, err
	}
	for _, port := range groupPorts {
		if port.DeviceID != "" || !port.CreatedAt.Before(before) {
			continue
		}
		if err := c.deletePortAndAddress(ctx, port.ID, nil); err != nil {
			errs = append(errs, fmt.Errorf("could not delete orphaned port %s: %w", port.ID, err))
			continue
		}
		deleted++
	}

	fips, err := c.listGroupAddresses(ctx, group)
	if err != nil {
		return deleted, errors.Join(append(errs, err)...)
	}
	for _, fip := range fips {
		if fip.PortID != "" || !fip.CreatedAt.Before(before) {
			continue
		}
		if err := floatingips.Delete(ctx, c.network, fip.ID).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("could not delete orphaned floating ip %s: %w", fip.ID, err))
			continue
		}
		deleted++
	}

	return deleted, errors.Join(errs...)
}

// deletePortAndAddress удаляет floating IP порта и сам порт; уже удалённые не
// считаются ошибкой. fip — если адрес уже известен, иначе ищется по порту.
func (c *openStack) deletePortAndAddress(ctx context.Context, portID string, fip *floatingips.FloatingIP) error {
	var fips []floatingips.FloatingIP
	if fip != nil {
		fips = []floatingips.FloatingIP{*fip}
	} else {
		pages, err := floatingips.List(c.network, floatingips.ListOpts{PortID: portID}).AllPages(ctx)
		if err != nil {
			return mapError(err)
		}
		if fips, err = floatingips.ExtractFloatingIPs(pages); err != nil {
			return err
		}
	}

	for _, f := range fips {
		if err := floatingips.Delete(ctx, c.network, f.ID).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			return fmt.Errorf("could not delete floating ip %s: %w", f.ID, mapError(err))
		}
	}

	if err := ports.Delete(ctx, c.network, portID).ExtractErr(); err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return mapError(err)
	}
	return nil
}

func (c *openStack) listGroupPorts(ctx context.Context, group, deviceID string) ([]ports.Port, error) {
	pages, err := ports.List(c.network, ports.ListOpts{Description: groupMarker(group), DeviceID: deviceID}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not list ports: %w", mapError(err))
	}
	return ports.ExtractPorts(pages)
}

func (c *openStack) listGroupAddresses(ctx context.Context, group string) ([]floatingips.FloatingIP, error) {
	pages, err := floatingips.List(c.network, floatingips.ListOpts{Description: groupMarker(group)}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not list floating ips: %w", mapError(err))
	}
	return floatingips.ExtractFloatingIPs(pages)
}

func (c *openStack) LatestImageByName(ctx context.Context, name, zone string) (string, error) {
	pages, err := images.List(c.image, images.ListOpts{Name: name, Status: images.ImageStatusActive}).AllPages(ctx)
	if err != nil {
		return "", mapError(err)
	}
	list, err := images.ExtractImages(pages)
	if err != nil {
		return "", err
	}

	var latest *images.Image
	for i := range list {
		if !inStore(list[i], zone) {
			continue
		}
		if latest == nil || list[i].CreatedAt.After(latest.CreatedAt) {
			latest = &list[i]
		}
	}
	if latest == nil {
		return "", fmt.Errorf("%w: no active image named %q in zone %s", ErrNotFound, name, zone)
	}
	return latest.ID, nil
}

// inStore — лежит ли образ в сторе зоны. Без поля stores (glance с одним
// стором) образ доступен везде.
func inStore(image images.Image, zone string) bool {
	stores, _ := image.Properties["stores"].(string)
	if stores == "" {
		return true
	}
	for _, store := range strings.Split(stores, ",") {
		if strings.TrimSpace(store) == zone {
			return true
		}
	}
	return false
}

func (c *openStack) listFlavors(ctx context.Context) ([]flavors.Flavor, error) {
	pages, err := flavors.ListDetail(c.compute, flavors.ListOpts{AccessType: flavors.PublicAccess}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not list flavors: %w", mapError(err))
	}
	return flavors.ExtractFlavors(pages)
}

func (c *openStack) FlavorID(ctx context.Context, nameOrID string) (string, error) {
	list, err := c.listFlavors(ctx)
	if err != nil {
		return "", err
	}

	for _, f := range list {
		if f.ID == nameOrID || f.Name == nameOrID {
			return f.ID, nil
		}
	}
	return "", fmt.Errorf("%w: flavor %q", ErrNotFound, nameOrID)
}

// CustomFlavor переиспользует только свои флейворы: публичный с теми же
// vCPU/RAM может оказаться другой линейкой (выделенные ядра, GPU) и другой
// ценой. Имя со случайным хвостом: Nova не даёт повторить имя даже
// удалённого флейвора.
func (c *openStack) CustomFlavor(ctx context.Context, vcpus, ramMB int) (string, error) {
	list, err := c.listFlavors(ctx)
	if err != nil {
		return "", err
	}

	for _, f := range list {
		if strings.HasPrefix(f.Name, customFlavorPrefix) && !f.IsPublic && f.VCPUs == vcpus && f.RAM == ramMB && f.Disk == 0 {
			return f.ID, nil
		}
	}

	disk := 0
	public := false
	flavor, err := flavors.Create(ctx, c.compute, flavors.CreateOpts{
		Name:     fmt.Sprintf("%s%d-%d-%s", customFlavorPrefix, vcpus, ramMB, randomHex()),
		VCPUs:    vcpus,
		RAM:      ramMB,
		Disk:     &disk,
		IsPublic: &public,
	}).Extract()
	if err != nil {
		return "", fmt.Errorf("could not create flavor: %w", mapError(err))
	}
	return flavor.ID, nil
}

func (c *openStack) ExternalNetworkID(ctx context.Context) (string, error) {
	yes := true
	pages, err := networks.List(c.network, external.ListOptsExt{
		ListOptsBuilder: networks.ListOpts{},
		External:        &yes,
	}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("could not list external networks: %w", mapError(err))
	}
	list, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", err
	}

	switch len(list) {
	case 0:
		return "", fmt.Errorf("%w: no external network in the project", ErrNotFound)
	case 1:
		return list[0].ID, nil
	default:
		names := make([]string, 0, len(list))
		for _, n := range list {
			names = append(names, n.Name+" ("+n.ID+")")
		}
		return "", fmt.Errorf("several external networks, set floating_network_id: %s", strings.Join(names, ", "))
	}
}

func (c *openStack) CreateKeypair(ctx context.Context, name, publicKey string) error {
	_, err := keypairs.Create(ctx, c.compute, keypairs.CreateOpts{Name: name, PublicKey: publicKey}).Extract()
	if err != nil {
		return fmt.Errorf("could not import keypair %s: %w", name, mapError(err))
	}
	return nil
}

func (c *openStack) DeleteKeypair(ctx context.Context, name string) error {
	err := keypairs.Delete(ctx, c.compute, name, nil).ExtractErr()
	if err != nil && !gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
		return fmt.Errorf("could not delete keypair %s: %w", name, mapError(err))
	}
	return nil
}

// quotaRegexp: Nova отвечает на превышение квоты 403 «Quota exceeded»,
// Neutron — 409 «OverQuota», Cinder в составе Nova — «VolumeLimitExceeded».
var quotaRegexp = regexp.MustCompile(`(?i)quota exceeded|overquota|limitexceeded`)

// mapError добавляет к ошибке HTTP наш sentinel, исходная остаётся в цепочке.
func mapError(err error) error {
	var codeErr gophercloud.ErrUnexpectedResponseCode
	if !errors.As(err, &codeErr) {
		return err
	}

	switch {
	case codeErr.Actual == http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case quotaRegexp.Match(codeErr.Body):
		return fmt.Errorf("%w: %w", ErrResourceExhausted, err)
	case codeErr.Actual >= http.StatusInternalServerError:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		return err
	}
}

func groupMarker(group string) string {
	return "fleeting-group=" + group
}

// address — элемент addresses сервера.
type address struct {
	Addr    string
	Version int
	Type    string
}

func fromServer(server *servers.Server) Instance {
	result := Instance{
		ID:       server.ID,
		Name:     server.Name,
		Status:   server.Status,
		Metadata: server.Metadata,
		Fault:    server.Fault.Message,
	}

	var fixed []string
	for _, raw := range server.Addresses {
		list, ok := raw.([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			a := toAddress(item)
			if a.Version != 4 {
				continue
			}
			switch a.Type {
			case "floating":
				if result.ExternalIP == "" {
					result.ExternalIP = a.Addr
				}
			default:
				fixed = append(fixed, a.Addr)
			}
		}
	}

	for _, addr := range fixed {
		ip, err := netip.ParseAddr(addr)
		if err != nil {
			continue
		}
		if ip.IsPrivate() {
			if result.InternalIP == "" {
				result.InternalIP = addr
			}
		} else if result.ExternalIP == "" {
			// порт прямо в публичной подсети
			result.ExternalIP = addr
		}
	}
	if result.InternalIP == "" && len(fixed) > 0 {
		result.InternalIP = fixed[0]
	}

	return result
}

func toAddress(item any) address {
	m, _ := item.(map[string]any)
	a := address{}
	a.Addr, _ = m["addr"].(string)
	if v, ok := m["version"].(float64); ok {
		a.Version = int(v)
	}
	a.Type, _ = m["OS-EXT-IPS:type"].(string)
	return a
}

func randomHex() string {
	b := make([]byte, 4)
	// crypto/rand.Read с Go 1.24 не возвращает ошибку
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
