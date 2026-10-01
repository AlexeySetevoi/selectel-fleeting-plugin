package selectel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	"github.com/AlexeySetevoi/selectel-fleeting-plugin/internal/selectelapi"
)

// fakeCloud — selectelapi.Cloud в памяти. create решает судьбу запроса сразу
// (квота, права), opErr — судьбу уже принятого сервера (нехватка ресурсов
// зоны); пока hold не закрыт, ожидание ACTIVE не завершается. Все запросы на
// создание и удаление записываются.
type fakeCloud struct {
	mu sync.Mutex

	instances []selectelapi.Instance
	create    func(req selectelapi.CreateInstanceRequest) error
	opErr     func(req selectelapi.CreateInstanceRequest) error
	hold      chan struct{}
	deleteErr map[string]error
	listErr   error
	images    map[string]string
	flavors   map[string]string
	external  string

	orphans    int
	orphansErr error

	created       []selectelapi.CreateInstanceRequest
	deleted       []string
	orphanChecks  []time.Time
	customFlavors []string
	keypairs      map[string]string
}

type fakeOperation struct {
	instance selectelapi.Instance
	err      error
	hold     chan struct{}
}

func (o *fakeOperation) InstanceID() string { return o.instance.ID }

func (o *fakeOperation) Wait(ctx context.Context) (*selectelapi.Instance, error) {
	if o.hold != nil {
		select {
		case <-o.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if o.err != nil {
		return nil, o.err
	}

	instance := o.instance
	instance.Status = selectelapi.StatusActive
	return &instance, nil
}

func (f *fakeCloud) ListInstances(_ context.Context, _ string) ([]selectelapi.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances, f.listErr
}

func (f *fakeCloud) GetInstance(_ context.Context, id string) (*selectelapi.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, instance := range f.instances {
		if instance.ID == id {
			return &instance, nil
		}
	}
	return nil, fmt.Errorf("%w: server %s", selectelapi.ErrNotFound, id)
}

func (f *fakeCloud) CreateInstance(_ context.Context, req selectelapi.CreateInstanceRequest) (selectelapi.CreateOperation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.created = append(f.created, req)
	if f.create != nil {
		if err := f.create(req); err != nil {
			return nil, err
		}
	}

	// как в облаке: принятый запрос сразу даёт сервер в BUILD
	instance := selectelapi.Instance{
		ID:       fmt.Sprintf("id-%d", len(f.created)),
		Name:     req.Name,
		Status:   selectelapi.StatusBuild,
		Metadata: req.Metadata,
	}
	f.instances = append(f.instances, instance)

	op := &fakeOperation{instance: instance, hold: f.hold}
	if f.opErr != nil {
		op.err = f.opErr(req)
	}
	return op, nil
}

func (f *fakeCloud) DeleteInstance(_ context.Context, id, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.deleteErr[id]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeCloud) CleanupOrphans(_ context.Context, _ string, before time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.orphanChecks = append(f.orphanChecks, before)
	return f.orphans, f.orphansErr
}

func (f *fakeCloud) LatestImageByName(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	id, ok := f.images[name]
	if !ok {
		return "", fmt.Errorf("%w: image %s", selectelapi.ErrNotFound, name)
	}
	return id, nil
}

func (f *fakeCloud) FlavorID(_ context.Context, name string) (string, error) {
	id, ok := f.flavors[name]
	if !ok {
		return "", fmt.Errorf("%w: flavor %s", selectelapi.ErrNotFound, name)
	}
	return id, nil
}

func (f *fakeCloud) CustomFlavor(_ context.Context, vcpus, ramMB int) (string, error) {
	id := fmt.Sprintf("custom-%d-%d", vcpus, ramMB)
	f.customFlavors = append(f.customFlavors, id)
	return id, nil
}

func (f *fakeCloud) ExternalNetworkID(context.Context) (string, error) {
	if f.external == "" {
		return "", fmt.Errorf("%w: no external network", selectelapi.ErrNotFound)
	}
	return f.external, nil
}

func (f *fakeCloud) CreateKeypair(_ context.Context, name, publicKey string) error {
	if f.keypairs == nil {
		f.keypairs = map[string]string{}
	}
	f.keypairs[name] = publicKey
	return nil
}

func (f *fakeCloud) DeleteKeypair(_ context.Context, name string) error {
	delete(f.keypairs, name)
	return nil
}

func validGroup() *InstanceGroup {
	return &InstanceGroup{
		Name:             "runner",
		AccountID:        "123456",
		ProjectID:        "project-1",
		Username:         "fleeting",
		Region:           "ru-9",
		AvailabilityZone: "ru-9a",
		Flavor:           "SL1.2-4096",
		ImageID:          "image-1",
		DiskSizeGB:       30,
		NetworkID:        "net-1",
		password:         "secret",
	}
}

func newFake() *fakeCloud {
	return &fakeCloud{flavors: map[string]string{
		"SL1.2-4096": "flavor-sl1",
		"SL1.4-8192": "flavor-sl1-big",
	}}
}

// clearEnv — чтобы RC-файл, загруженный в шелл разработчика, не попал в тесты.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, env := range []string{envAuthURL, envAccountID, envProjectID, envUsername, envPassword, envRegion} {
		t.Setenv(env, "")
	}
}

func stubCloud(t *testing.T, fake *fakeCloud) *selectelapi.Auth {
	t.Helper()
	clearEnv(t)

	var auth selectelapi.Auth
	orig := newCloud
	newCloud = func(_ context.Context, a selectelapi.Auth) (selectelapi.Cloud, error) {
		auth = a
		return fake, nil
	}
	t.Cleanup(func() { newCloud = orig })
	return &auth
}

// initGroup прогоняет настоящий Init с подменённым клиентом.
func initGroup(t *testing.T, g *InstanceGroup, fake *fakeCloud, settings provider.Settings) {
	t.Helper()

	stubCloud(t, fake)
	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	// гасит фоновые горутины watch, чтобы они не переживали тест
	t.Cleanup(func() { _ = g.Shutdown(context.Background()) })
}

func TestInitProviderInfo(t *testing.T) {
	g := validGroup()
	auth := stubCloud(t, newFake())

	info, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if info.ID != "selectel/project-1/ru-9/runner" {
		t.Fatalf("ProviderInfo.ID = %q", info.ID)
	}
	// раннер пишет их в лог при старте — по ним видно, какая сборка плагина стоит
	if info.Version == "" || info.BuildInfo == "" {
		t.Fatalf("ProviderInfo version = %q, build info = %q, want both set", info.Version, info.BuildInfo)
	}

	want := selectelapi.Auth{
		AuthURL:   defaultAuthURL,
		AccountID: "123456",
		Username:  "fleeting",
		Password:  "secret",
		ProjectID: "project-1",
		Region:    "ru-9",
	}
	if *auth != want {
		t.Fatalf("auth = %+v, want %+v", *auth, want)
	}
}

// Переменные из RC-файла Selectel важнее конфига.
func TestInitCredentialsFromEnv(t *testing.T) {
	g := validGroup()
	g.password = ""
	g.PasswordFile = "/nonexistent/password"
	auth := stubCloud(t, newFake())

	t.Setenv(envAuthURL, "https://example.test/identity/v3")
	t.Setenv(envAccountID, "999")
	t.Setenv(envProjectID, "project-env")
	t.Setenv(envUsername, "env-user")
	t.Setenv(envPassword, "env-secret")
	t.Setenv(envRegion, "ru-7")

	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatalf("Init() = %v", err)
	}

	want := selectelapi.Auth{
		AuthURL:   "https://example.test/identity/v3",
		AccountID: "999",
		Username:  "env-user",
		Password:  "env-secret",
		ProjectID: "project-env",
		Region:    "ru-7",
	}
	if *auth != want {
		t.Fatalf("auth = %+v, want %+v", *auth, want)
	}
}

func TestInitPasswordFile(t *testing.T) {
	file := t.TempDir() + "/password"
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := validGroup()
	g.password = ""
	g.PasswordFile = file
	auth := stubCloud(t, newFake())

	if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err != nil {
		t.Fatalf("Init() = %v", err)
	}
	if auth.Password != "from-file" {
		t.Fatalf("password = %q, want the trimmed file content", auth.Password)
	}
}

func TestUpdateFiltersByMetadata(t *testing.T) {
	fake := newFake()
	fake.instances = []selectelapi.Instance{
		{ID: "a", Status: selectelapi.StatusActive, Metadata: map[string]string{metadataGroup: "runner"}},
		{ID: "b", Status: selectelapi.StatusBuild, Metadata: map[string]string{metadataGroup: "runner"}},
		// префикс имени совпал, но группа другая (runner-x-...)
		{ID: "c", Status: selectelapi.StatusActive, Metadata: map[string]string{metadataGroup: "runner-x"}},
		// имя похоже, но metadata нет — не наш
		{ID: "d", Name: "runner-aaaa1111", Status: selectelapi.StatusActive},
		{ID: "e", Status: "SOMETHING_NEW", Metadata: map[string]string{metadataGroup: "runner"}},
		{ID: "f", Status: selectelapi.StatusExpired, Metadata: map[string]string{metadataGroup: "runner"}},
	}

	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	got := map[string]provider.State{}
	if err := g.Update(context.Background(), func(instance string, state provider.State) {
		got[instance] = state
	}); err != nil {
		t.Fatalf("Update() = %v", err)
	}

	want := map[string]provider.State{
		"a": provider.StateRunning,
		"b": provider.StateCreating,
		"f": provider.StateTimeout,
	}
	if len(got) != len(want) {
		t.Fatalf("Update() reported %v, want %v", got, want)
	}
	for id, state := range want {
		if got[id] != state {
			t.Fatalf("Update() state[%s] = %q, want %q", id, got[id], state)
		}
	}
}

// Сирот ищем не на каждый Update, а раз в orphanCheckInterval, и только
// старше OrphanAge.
func TestUpdateCleansUpOrphans(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	fake := newFake()
	fake.orphans = 2

	g := validGroup()
	g.now = func() time.Time { return now }
	initGroup(t, g, fake, provider.Settings{})

	update := func() {
		t.Helper()
		if err := g.Update(context.Background(), func(string, provider.State) {}); err != nil {
			t.Fatalf("Update() = %v", err)
		}
	}

	update()
	update()
	now = now.Add(orphanCheckInterval)
	update()

	if len(fake.orphanChecks) != 2 {
		t.Fatalf("orphan checks = %d, want 2", len(fake.orphanChecks))
	}
	if want := now.Add(-selectelapi.OrphanAge); !fake.orphanChecks[1].Equal(want) {
		t.Fatalf("orphan cutoff = %v, want %v", fake.orphanChecks[1], want)
	}
}

// Сбой уборки виден раннеру, но состояния серверов он уже получил.
func TestUpdateOrphanCleanupError(t *testing.T) {
	fake := newFake()
	fake.orphansErr = errors.New("neutron unavailable")
	fake.instances = []selectelapi.Instance{
		{ID: "a", Status: selectelapi.StatusActive, Metadata: map[string]string{metadataGroup: "runner"}},
	}

	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	var got []string
	err := g.Update(context.Background(), func(id string, _ provider.State) { got = append(got, id) })
	if err == nil || !strings.Contains(err.Error(), "neutron unavailable") {
		t.Fatalf("Update() = %v, want the cleanup error", err)
	}
	if len(got) != 1 {
		t.Fatalf("Update() reported %v before failing", got)
	}
}

func TestIncreaseRequest(t *testing.T) {
	fake := newFake()
	fake.images = map[string]string{"Ubuntu 24.04 LTS 64-bit": "image-latest"}
	fake.external = "ext-net"

	g := validGroup()
	g.ImageID = ""
	g.ImageName = "Ubuntu 24.04 LTS 64-bit"
	g.SubnetID = "subnet-1"
	g.SecurityGroupIDs = []string{"sg-1"}
	g.FloatingIP = true
	g.Preemptible = true
	g.Tags = []string{"ci"}
	g.Metadata = map[string]string{"team": "platform"}
	g.UserData = "#cloud-config\n"
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if err != nil || succeeded != 1 {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}

	req := fake.created[0]

	if !strings.HasPrefix(req.Name, "runner-") || len(req.Name) != len("runner-")+8 {
		t.Fatalf("server name = %q", req.Name)
	}
	if req.ImageID != "image-latest" {
		t.Fatalf("ImageID = %q, want the one resolved by name", req.ImageID)
	}
	if req.FlavorID != "flavor-sl1" || req.AvailabilityZone != "ru-9a" || req.DiskSizeGB != 30 {
		t.Fatalf("flavor/zone/disk = %q/%q/%d", req.FlavorID, req.AvailabilityZone, req.DiskSizeGB)
	}
	if req.VolumeType != "universal.ru-9a" {
		t.Fatalf("VolumeType = %q", req.VolumeType)
	}
	if req.NetworkID != "net-1" || req.SubnetID != "subnet-1" || !slices.Equal(req.SecurityGroupIDs, []string{"sg-1"}) {
		t.Fatalf("network = %q/%q/%v", req.NetworkID, req.SubnetID, req.SecurityGroupIDs)
	}
	if req.FloatingNetworkID != "ext-net" {
		t.Fatalf("FloatingNetworkID = %q, want the discovered external network", req.FloatingNetworkID)
	}
	if !slices.Equal(req.Tags, []string{"ci", tagPreemptible}) {
		t.Fatalf("tags = %v", req.Tags)
	}
	if req.Group != "runner" || req.UserData != "#cloud-config\n" {
		t.Fatalf("group = %q, user data = %q", req.Group, req.UserData)
	}

	wantMetadata := map[string]string{"team": "platform", metadataGroup: "runner", metadataManagedBy: NAME}
	for k, v := range wantMetadata {
		if req.Metadata[k] != v {
			t.Fatalf("metadata %s = %q, want %q", k, req.Metadata[k], v)
		}
	}
	if _, ok := g.Metadata[metadataGroup]; ok {
		t.Fatal("instanceMetadata() modified the configured metadata")
	}
	if len(g.Tags) != 1 {
		t.Fatal("instanceTags() modified the configured tags")
	}

	if !strings.HasPrefix(req.KeyName, "fleeting-runner-") {
		t.Fatalf("KeyName = %q", req.KeyName)
	}
	if !strings.HasPrefix(fake.keypairs[req.KeyName], "ssh-ed25519 ") {
		t.Fatalf("keypair %s = %q", req.KeyName, fake.keypairs[req.KeyName])
	}
	if len(g.settings.Key) == 0 {
		t.Fatal("generated private key was not stored in connector settings")
	}
}

func TestIncreaseExplicitVolumeTypeAndFloatingNetwork(t *testing.T) {
	fake := newFake()
	g := validGroup()
	g.VolumeType = "fast.ru-9b"
	g.FloatingIP = true
	g.FloatingNetworkID = "ext-chosen"
	initGroup(t, g, fake, provider.Settings{})

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if req := fake.created[0]; req.VolumeType != "fast.ru-9b" || req.FloatingNetworkID != "ext-chosen" {
		t.Fatalf("VolumeType = %q, FloatingNetworkID = %q", req.VolumeType, req.FloatingNetworkID)
	}
}

func TestIncreaseWithoutFloatingIP(t *testing.T) {
	fake := newFake()
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if req := fake.created[0]; req.FloatingNetworkID != "" || len(req.Tags) != 0 {
		t.Fatalf("FloatingNetworkID = %q, tags = %v", req.FloatingNetworkID, req.Tags)
	}
}

func TestInitNoExternalNetwork(t *testing.T) {
	g := validGroup()
	g.FloatingIP = true
	stubCloud(t, newFake())

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if !errors.Is(err, selectelapi.ErrNotFound) {
		t.Fatalf("Init() = %v, want not found", err)
	}
}

func TestCustomFlavor(t *testing.T) {
	fake := newFake()
	g := validGroup()
	g.Flavor = ""
	g.Cores = 4
	g.MemoryGB = 0.5
	g.AvailabilityZone = ""
	g.Placements = []Placement{{AvailabilityZone: "ru-9a"}, {AvailabilityZone: "ru-9b"}}
	initGroup(t, g, fake, provider.Settings{})

	if !slices.Equal(fake.customFlavors, []string{"custom-4-512"}) {
		t.Fatalf("custom flavors = %v, want one shared by all placements", fake.customFlavors)
	}

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if req := fake.created[0]; req.FlavorID != "custom-4-512" {
		t.Fatalf("FlavorID = %q", req.FlavorID)
	}
}

func TestInitUnknownFlavor(t *testing.T) {
	g := validGroup()
	g.Flavor = "SL1.no-such"
	stubCloud(t, newFake())

	_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
	if !errors.Is(err, selectelapi.ErrNotFound) {
		t.Fatalf("Init() = %v, want not found", err)
	}
}

func TestIncreaseWinRMHasNoSSHKey(t *testing.T) {
	fake := newFake()
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Protocol:             provider.ProtocolWinRM,
		Password:             "secret",
		UseStaticCredentials: true,
	}})

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if fake.created[0].KeyName != "" || len(fake.keypairs) != 0 {
		t.Fatal("keypair set for a winrm group")
	}
	if g.settings.Username != "Administrator" {
		t.Fatalf("Username = %q", g.settings.Username)
	}
}

func placementsGroup() *InstanceGroup {
	g := validGroup()
	g.AvailabilityZone, g.Flavor = "", ""
	g.Placements = []Placement{
		{AvailabilityZone: "ru-9a", Flavor: "SL1.2-4096"},
		{AvailabilityZone: "ru-9b", Flavor: "SL1.4-8192"},
	}
	return g
}

func TestIncreasePlacementFallback(t *testing.T) {
	fake := newFake()
	fake.create = func(req selectelapi.CreateInstanceRequest) error {
		if req.AvailabilityZone == "ru-9a" {
			return fmt.Errorf("%w: quota exceeded", selectelapi.ErrResourceExhausted)
		}
		return nil
	}

	g := placementsGroup()
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if err != nil || succeeded != 1 {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}
	if len(fake.created) != 2 {
		t.Fatalf("create attempts = %d, want 2", len(fake.created))
	}

	second := fake.created[1]
	if second.AvailabilityZone != "ru-9b" || second.FlavorID != "flavor-sl1-big" || second.VolumeType != "universal.ru-9b" {
		t.Fatalf("second attempt = %+v", second)
	}
	if fake.created[0].Name == second.Name {
		t.Fatal("server name reused across attempts")
	}
}

func TestIncreaseNoFallbackOnOtherErrors(t *testing.T) {
	fake := newFake()
	fake.create = func(selectelapi.CreateInstanceRequest) error {
		return errors.New("forbidden")
	}

	g := placementsGroup()
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if err == nil || succeeded != 0 {
		t.Fatalf("Increase() = %d, %v, want an error", succeeded, err)
	}
	if len(fake.created) != 1 {
		t.Fatalf("create attempts = %d, want 1", len(fake.created))
	}
}

func TestIncreasePartialSuccess(t *testing.T) {
	var calls int
	fake := newFake()
	// create вызывается под мьютексом фейка
	fake.create = func(selectelapi.CreateInstanceRequest) error {
		calls++
		if calls%2 == 0 {
			return fmt.Errorf("%w: quota", selectelapi.ErrResourceExhausted)
		}
		return nil
	}

	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 8)
	if succeeded != 4 {
		t.Fatalf("Increase() succeeded = %d, want 4", succeeded)
	}
	if !errors.Is(err, selectelapi.ErrResourceExhausted) {
		t.Fatalf("Increase() error = %v", err)
	}
}

func TestIncreaseImageNotFound(t *testing.T) {
	fake := newFake()
	g := validGroup()
	g.ImageID = ""
	g.ImageName = "No Such Image"
	initGroup(t, g, fake, provider.Settings{})

	succeeded, err := g.Increase(context.Background(), 1)
	if succeeded != 0 || !errors.Is(err, selectelapi.ErrNotFound) {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}
	if len(fake.created) != 0 {
		t.Fatal("server creation attempted without an image")
	}
}

func TestDecrease(t *testing.T) {
	fake := newFake()
	fake.deleteErr = map[string]error{
		"gone":   fmt.Errorf("%w: server gone", selectelapi.ErrNotFound),
		"broken": errors.New("internal error"),
	}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	deleted, err := g.Decrease(context.Background(), []string{"ok", "gone", "broken"})
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("Decrease() error = %v", err)
	}
	if len(deleted) != 2 || deleted[0] != "ok" || deleted[1] != "gone" {
		t.Fatalf("Decrease() deleted = %v", deleted)
	}
}

func TestConnectInfo(t *testing.T) {
	fake := newFake()
	fake.instances = []selectelapi.Instance{
		{ID: "a", InternalIP: "10.0.0.5", ExternalIP: "203.0.113.7"},
	}
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	info, err := g.ConnectInfo(context.Background(), "a")
	if err != nil {
		t.Fatalf("ConnectInfo() = %v", err)
	}
	if info.ID != "a" || info.InternalAddr != "10.0.0.5" || info.ExternalAddr != "203.0.113.7" {
		t.Fatalf("ConnectInfo() = %+v", info)
	}
	if info.Username != "root" || len(info.Key) == 0 {
		t.Fatalf("ConnectInfo() credentials: username=%q has_key=%v", info.Username, len(info.Key) > 0)
	}

	if _, err := g.ConnectInfo(context.Background(), "missing"); !errors.Is(err, selectelapi.ErrNotFound) {
		t.Fatalf("ConnectInfo(missing) = %v", err)
	}
}

func TestStaticSSHKey(t *testing.T) {
	pub, priv, err := generateSSHKeyPair()
	if err != nil {
		t.Fatal(err)
	}

	fake := newFake()
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Username:             "ci",
		Key:                  priv,
		UseStaticCredentials: true,
	}})

	if want := strings.TrimSpace(string(pub)); fake.keypairs[g.keyName] != want {
		t.Fatalf("keypair = %q, want %q", fake.keypairs[g.keyName], want)
	}
}

// Каждый запуск — своя keypair, Shutdown удаляет только её.
func TestShutdownDeletesKeypair(t *testing.T) {
	fake := newFake()
	fake.keypairs = map[string]string{"fleeting-runner-previous": "ssh-ed25519 AAAA"}

	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})
	if len(fake.keypairs) != 2 {
		t.Fatalf("keypairs after Init = %v", fake.keypairs)
	}

	if err := g.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
	if _, ok := fake.keypairs["fleeting-runner-previous"]; !ok || len(fake.keypairs) != 1 {
		t.Fatalf("keypairs after Shutdown = %v", fake.keypairs)
	}
}

func TestInitErrors(t *testing.T) {
	orig := newCloud
	t.Cleanup(func() { newCloud = orig })
	clearEnv(t)

	var built bool
	newCloud = func(context.Context, selectelapi.Auth) (selectelapi.Cloud, error) {
		built = true
		return newFake(), nil
	}

	t.Run("invalid config", func(t *testing.T) {
		g := validGroup()
		g.ProjectID = ""
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
			t.Fatal("Init() = nil for an invalid config")
		}
		if built {
			t.Fatal("api client built although the config is invalid")
		}
	})

	t.Run("unreadable password file", func(t *testing.T) {
		g := validGroup()
		g.password = ""
		g.PasswordFile = "/nonexistent/password"
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
			t.Fatal("Init() = nil for a missing password_file")
		}
	})

	t.Run("unreadable user data file", func(t *testing.T) {
		g := validGroup()
		g.UserDataFile = "/nonexistent/user-data.yml"
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{}); err == nil {
			t.Fatal("Init() = nil for a missing user_data_file")
		}
	})

	t.Run("invalid static ssh key", func(t *testing.T) {
		g := validGroup()
		settings := provider.Settings{ConnectorConfig: provider.ConnectorConfig{
			Key:                  []byte("not a key"),
			UseStaticCredentials: true,
		}}
		if _, err := g.Init(context.Background(), hclog.NewNullLogger(), settings); err == nil {
			t.Fatal("Init() = nil for an invalid static ssh key")
		}
	})

	t.Run("client cannot be built", func(t *testing.T) {
		newCloud = func(context.Context, selectelapi.Auth) (selectelapi.Cloud, error) {
			return nil, errors.New("could not authenticate in selectel keystone: 401")
		}

		g := validGroup()
		_, err := g.Init(context.Background(), hclog.NewNullLogger(), provider.Settings{})
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("Init() = %v, want the auth error", err)
		}
	})
}

// Статический пароль по SSH: ключа нет, keypair не нужна.
func TestStaticSSHPasswordHasNoKey(t *testing.T) {
	fake := newFake()
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{ConnectorConfig: provider.ConnectorConfig{
		Password:             "secret",
		UseStaticCredentials: true,
	}})

	if _, err := g.Increase(context.Background(), 1); err != nil {
		t.Fatalf("Increase() = %v", err)
	}
	if fake.created[0].KeyName != "" || len(fake.keypairs) != 0 {
		t.Fatal("keypair set without a key")
	}
	if len(g.settings.Key) != 0 {
		t.Fatal("a key was generated despite use_static_credentials")
	}
}

func TestUpdateListError(t *testing.T) {
	fake := newFake()
	fake.listErr = errors.New("unavailable")
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	called := false
	err := g.Update(context.Background(), func(string, provider.State) { called = true })
	if err == nil || called {
		t.Fatalf("Update() = %v, callback called = %v", err, called)
	}
}

func TestDecreaseEmpty(t *testing.T) {
	g := validGroup()
	initGroup(t, g, newFake(), provider.Settings{})

	deleted, err := g.Decrease(context.Background(), nil)
	if err != nil || len(deleted) != 0 {
		t.Fatalf("Decrease(nil) = %v, %v", deleted, err)
	}
}

func TestHeartbeatAndSuspendResume(t *testing.T) {
	g := validGroup()
	initGroup(t, g, newFake(), provider.Settings{})

	if err := g.Heartbeat(context.Background(), "a"); err != nil {
		t.Fatalf("Heartbeat() = %v", err)
	}
	if _, err := g.Suspend(context.Background(), []string{"a"}); !errors.Is(err, provider.ErrSuspendResumeNotSupported) {
		t.Fatalf("Suspend() = %v", err)
	}
	if _, err := g.Resume(context.Background(), []string{"a"}); !errors.Is(err, provider.ErrSuspendResumeNotSupported) {
		t.Fatalf("Resume() = %v", err)
	}
}

// Shutdown зовут и после неудачного Init, когда клиента ещё нет.
func TestShutdownWithoutInit(t *testing.T) {
	if err := validGroup().Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() = %v", err)
	}
}

func TestRandomSuffix(t *testing.T) {
	a, b := randomSuffix(), randomSuffix()
	if len(a) != 8 || a == b {
		t.Fatalf("randomSuffix() = %q, %q", a, b)
	}
}

// Цикл раннера однопоточный: пока Increase не вернулся, не идут ни Update, ни
// удаление. Поэтому он обязан вернуться, не дожидаясь ACTIVE.
func TestIncreaseDoesNotWaitForServers(t *testing.T) {
	fake := newFake()
	fake.hold = make(chan struct{})
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	type result struct {
		succeeded int
		err       error
	}
	done := make(chan result, 1)
	go func() {
		succeeded, err := g.Increase(context.Background(), 3)
		done <- result{succeeded, err}
	}()

	select {
	case r := <-done:
		if r.err != nil || r.succeeded != 3 {
			t.Fatalf("Increase() = %d, %v", r.succeeded, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Increase() is blocked on servers still in BUILD")
	}

	// принятые серверы уже видны раннеру как creating
	creating := 0
	if err := g.Update(context.Background(), func(_ string, state provider.State) {
		if state == provider.StateCreating {
			creating++
		}
	}); err != nil || creating != 3 {
		t.Fatalf("Update() reported %d creating instances, err %v, want 3", creating, err)
	}

	close(fake.hold)
	g.watchers.Wait()
}

// Нехватка ресурсов зоны приходит уже после того, как запрос принят (сервер
// уходит в ERROR с «No valid host»). Increase к этому моменту вернулся,
// поэтому fallback срабатывает на следующем запросе: размещение уходит в
// конец очереди, а через placementCooldown возвращается.
func TestAsyncExhaustionMovesPlacementToTheEnd(t *testing.T) {
	exhausted := true
	fake := newFake()
	fake.opErr = func(req selectelapi.CreateInstanceRequest) error {
		if exhausted && req.AvailabilityZone == "ru-9a" {
			return fmt.Errorf("%w: No valid host was found", selectelapi.ErrResourceExhausted)
		}
		return nil
	}

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	g := placementsGroup()
	g.now = func() time.Time { return now }
	initGroup(t, g, fake, provider.Settings{})

	increase := func() string {
		t.Helper()
		succeeded, err := g.Increase(context.Background(), 1)
		if err != nil || succeeded != 1 {
			t.Fatalf("Increase() = %d, %v", succeeded, err)
		}
		g.watchers.Wait()
		return fake.created[len(fake.created)-1].AvailabilityZone
	}

	if zone := increase(); zone != "ru-9a" {
		t.Fatalf("first request went to %s, want the first placement", zone)
	}
	if zone := increase(); zone != "ru-9b" {
		t.Fatalf("request after the async failure went to %s, want the next placement", zone)
	}

	exhausted = false
	now = now.Add(placementCooldown - time.Second)
	if zone := increase(); zone != "ru-9b" {
		t.Fatalf("request within the cooldown went to %s", zone)
	}

	now = now.Add(2 * time.Second)
	if zone := increase(); zone != "ru-9a" {
		t.Fatalf("request after the cooldown went to %s, want the first placement again", zone)
	}
}

// Любая другая ошибка от смены зоны не лечится: порядок не меняется.
func TestAsyncOtherErrorKeepsPlacementOrder(t *testing.T) {
	fake := newFake()
	fake.opErr = func(selectelapi.CreateInstanceRequest) error {
		return errors.New("server went to ERROR: image is broken")
	}
	g := placementsGroup()
	initGroup(t, g, fake, provider.Settings{})

	for range 2 {
		if succeeded, err := g.Increase(context.Background(), 1); err != nil || succeeded != 1 {
			t.Fatalf("Increase() = %d, %v", succeeded, err)
		}
		g.watchers.Wait()
	}

	for i, req := range fake.created {
		if req.AvailabilityZone != "ru-9a" {
			t.Fatalf("request #%d went to %s, want the first placement", i+1, req.AvailabilityZone)
		}
	}
}

// Размещения в конце очереди не выключены: если ресурсы кончились везде,
// пробуем все в порядке из конфига, а не отказываем раннеру без попытки.
func TestAllPlacementsExhaustedAreStillTried(t *testing.T) {
	fake := newFake()
	fake.opErr = func(selectelapi.CreateInstanceRequest) error {
		return fmt.Errorf("%w: No valid host was found", selectelapi.ErrResourceExhausted)
	}
	g := placementsGroup()
	initGroup(t, g, fake, provider.Settings{})

	var zones []string
	for range 3 {
		if succeeded, err := g.Increase(context.Background(), 1); err != nil || succeeded != 1 {
			t.Fatalf("Increase() = %d, %v", succeeded, err)
		}
		g.watchers.Wait()
		zones = append(zones, fake.created[len(fake.created)-1].AvailabilityZone)
	}

	want := []string{"ru-9a", "ru-9b", "ru-9a"}
	if strings.Join(zones, ",") != strings.Join(want, ",") {
		t.Fatalf("zones tried = %v, want %v", zones, want)
	}
}

// Остановка плагина не должна висеть на серверах, которые ещё создаются.
func TestShutdownDoesNotWaitForServers(t *testing.T) {
	fake := newFake()
	fake.hold = make(chan struct{})
	g := validGroup()
	initGroup(t, g, fake, provider.Settings{})

	if succeeded, err := g.Increase(context.Background(), 2); err != nil || succeeded != 2 {
		t.Fatalf("Increase() = %d, %v", succeeded, err)
	}

	done := make(chan error, 1)
	go func() { done <- g.Shutdown(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown() is blocked on servers still in BUILD")
	}

	// отменённое ожидание — не отказ зоны
	if got := g.orderedPlacements(); len(got) != 1 || len(g.exhaustedUntil) != 0 {
		t.Fatalf("placements after shutdown = %v, exhausted = %v", got, g.exhaustedUntil)
	}
}
