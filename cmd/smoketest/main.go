// Command smoketest гоняет InstanceGroup по настоящему облаку Selectel:
// Init -> Increase(1) -> опрос Update до Running -> пауза -> ConnectInfo ->
// реальный вход по SSH/WinRM -> Decrease -> Shutdown. Проверяет то, что
// юнит-тестами не покрыть: слой над gophercloud, реальные статусы серверов,
// коды ошибок и то, что данные из ConnectInfo действительно пускают на машину.
// Подключается тем же пакетом fleeting/connector, что и сам GitLab Runner.
//
// Учётные данные — из RC-файла сервисного пользователя (OS_USERNAME,
// OS_PASSWORD, OS_USER_DOMAIN_NAME, OS_PROJECT_ID, OS_REGION_NAME):
//
//	. ./rc.sh && go run ./cmd/smoketest \
//	    -availability-zone=ru-9a -flavor=SL1.1-2048 -network-id=... \
//	    -image-name="Ubuntu 24.04 LTS 64-bit" -floating-ip
//
// Посмотреть реальный код ошибки квоты (сервер не создаётся) — заведомо
// невыполнимый запрос:
//
//	... -cores=200 -memory-gb=1024
//
// Windows/WinRM (пароль уже задан в образе или через -user-data-file):
//
//	... -image-id=... -protocol=winrm -username=Administrator -password=...
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"

	"gitlab.com/gitlab-org/fleeting/fleeting/connector"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"

	selectel "github.com/AlexeySetevoi/selectel-fleeting-plugin"
)

const accessCheckAttemptTimeout = 20 * time.Second

func main() {
	os.Exit(run())
}

// run — отдельно от main, чтобы defer-ы (удаление созданной ВМ) отработали до
// выхода из процесса.
func run() int {
	name := flag.String("name", "smoketest", "instance group name / server name prefix")
	count := flag.Int("count", 1, "how many servers to request with a single Increase call")
	passwordFile := flag.String("password-file", "", "service user password file (or OS_PASSWORD)")
	zone := flag.String("availability-zone", "", "availability zone, e.g. ru-9a")
	flavor := flag.String("flavor", "", "flavor name or id, e.g. SL1.1-2048")
	placements := flag.String("placements", "", "comma separated zone[:flavor] fallback list, instead of -availability-zone/-flavor")
	placementStrategy := flag.String("placement-strategy", "", "ordered (default), round_robin or random")
	cores := flag.Int("cores", 0, "vCPU count for a custom flavor, instead of -flavor")
	memoryGB := flag.Float64("memory-gb", 0, "RAM for a custom flavor, GB")
	imageID := flag.String("image-id", "", "image id")
	imageName := flag.String("image-name", "", "image name (alternative to -image-id), e.g. \"Ubuntu 24.04 LTS 64-bit\"")
	volumeType := flag.String("volume-type", "", "boot volume type without zone (default universal)")
	diskSizeGB := flag.Int("disk-size-gb", 10, "boot volume size, GB")
	networkID := flag.String("network-id", "", "network id (required)")
	subnetID := flag.String("subnet-id", "", "subnet id in that network")
	securityGroupIDs := flag.String("security-group-ids", "", "comma separated security group ids")
	floatingIP := flag.Bool("floating-ip", false, "give the server a floating ip")
	preemptible := flag.Bool("preemptible", false, "create a preemptible server")
	userDataFile := flag.String("user-data-file", "", "path to cloud-init / cloudbase-init user-data")
	protocol := flag.String("protocol", "ssh", "connector protocol: ssh, winrm or winrm+https")
	username := flag.String("username", "", "connector username (default root / Administrator)")
	password := flag.String("password", "", "static connector password (required for winrm)")
	useExternalAddr := flag.Bool("use-external-addr", true, "connect to the public address; set false when the subnet is reachable directly")
	pollInterval := flag.Duration("poll-interval", 10*time.Second, "how often to call Update while waiting for the instance")
	readyTimeout := flag.Duration("ready-timeout", 5*time.Minute, "how long to wait for state Running")
	settle := flag.Duration("settle", 30*time.Second, "pause between Running and the access check")
	accessTimeout := flag.Duration("access-timeout", 3*time.Minute, "how long to keep retrying the access check")
	accessRetryInterval := flag.Duration("access-retry-interval", 10*time.Second, "delay between access check attempts")
	checkCmd := flag.String("check-cmd", "", "override the access check command")
	keep := flag.Bool("keep", false, "skip Decrease/Shutdown, leave the instance for manual inspection")
	flag.Parse()

	if *networkID == "" {
		fmt.Fprintln(os.Stderr, "usage: smoketest -network-id=<id> (-availability-zone=<zone> -flavor=<flavor> | -placements=...) (-image-id=<id> | -image-name=<name>) ...")
		flag.PrintDefaults()
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	logger := hclog.New(&hclog.LoggerOptions{
		Name:  "smoketest",
		Level: hclog.Debug,
	})

	g := &selectel.InstanceGroup{
		Name:              *name,
		PasswordFile:      *passwordFile,
		AvailabilityZone:  *zone,
		Flavor:            *flavor,
		Placements:        parsePlacements(*placements),
		PlacementStrategy: *placementStrategy,
		Cores:             *cores,
		MemoryGB:          *memoryGB,
		ImageID:           *imageID,
		ImageName:         *imageName,
		VolumeType:        *volumeType,
		DiskSizeGB:        *diskSizeGB,
		NetworkID:         *networkID,
		SubnetID:          *subnetID,
		FloatingIP:        *floatingIP,
		Preemptible:       *preemptible,
		UserDataFile:      *userDataFile,
	}
	if *securityGroupIDs != "" {
		g.SecurityGroupIDs = strings.Split(*securityGroupIDs, ",")
	}

	settings := provider.Settings{
		ConnectorConfig: provider.ConnectorConfig{
			Protocol:             provider.Protocol(*protocol),
			Username:             *username,
			Password:             *password,
			UseStaticCredentials: *password != "",
			Timeout:              15 * time.Second,
			Keepalive:            10 * time.Second,
		},
	}

	info, err := g.Init(ctx, logger, settings)
	if err != nil {
		logger.Error("init failed", "error", err)
		return 1
	}
	logger.Info("initialized", "provider_id", info.ID, "max_size", info.MaxSize)

	increaseStarted := time.Now()
	succeeded, err := g.Increase(ctx, *count)
	logger.Info("increase returned", "requested", *count, "succeeded", succeeded, "took", time.Since(increaseStarted).Round(time.Millisecond).String())
	if err != nil {
		logger.Error("increase reported an error", "error", err)
	}

	// дальше серверы существуют и должны быть убраны при любом исходе, в том
	// числе те, что создались при частичном успехе
	defer func() {
		ids := groupInstances(logger, g)
		if *keep {
			logger.Info("keeping instances and the keypair alive (-keep set); clean them up manually", "ids", ids)
			return
		}

		if len(ids) > 0 {
			deleted, err := g.Decrease(context.Background(), ids)
			if err != nil {
				logger.Error("decrease reported an error", "error", err)
			}
			logger.Info("decreased", "deleted", deleted)
		}

		// Shutdown удаляет keypair запуска — и когда серверов нет
		if err := g.Shutdown(context.Background()); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
	}()

	if succeeded != *count {
		logger.Error("increase did not create all instances", "succeeded", succeeded, "requested", *count)
		return 1
	}

	ids, err := waitUntilRunning(ctx, logger, g, *count, *pollInterval, *readyTimeout)
	if err != nil {
		logger.Error("waiting for instances failed", "error", err)
		return 1
	}
	logger.Info("all instances are running", "count", len(ids), "since_increase", time.Since(increaseStarted).Round(time.Second).String())

	logger.Info("letting instances settle before checking access", "duration", settle.String())
	select {
	case <-ctx.Done():
		return 1
	case <-time.After(*settle):
	}

	failed := false
	for _, id := range ids {
		connectInfo, err := g.ConnectInfo(ctx, id)
		if err != nil {
			logger.Error("connect info failed", "id", id, "error", err)
			failed = true
			continue
		}
		logger.Info("connect info",
			"id", id,
			"external_addr", connectInfo.ExternalAddr,
			"internal_addr", connectInfo.InternalAddr,
			"username", connectInfo.Username,
			"protocol", connectInfo.Protocol,
			"has_key", len(connectInfo.Key) > 0,
			"has_password", connectInfo.Password != "",
		)

		if err := checkAccess(ctx, logger, connectInfo, *accessTimeout, *accessRetryInterval, *checkCmd, *useExternalAddr); err != nil {
			logger.Error("ACCESS CHECK FAILED", "id", id, "error", err)
			failed = true
		}
	}
	if failed {
		return 1
	}

	logger.Info("ACCESS CHECK PASSED", "instances", len(ids))
	return 0
}

// groupInstances — все инстансы группы, какие сейчас видит Update.
func groupInstances(logger hclog.Logger, g *selectel.InstanceGroup) []string {
	var ids []string
	if err := g.Update(context.Background(), func(instance string, _ provider.State) {
		ids = append(ids, instance)
	}); err != nil {
		logger.Error("update failed", "error", err)
	}
	return ids
}

func parsePlacements(value string) []selectel.Placement {
	if value == "" {
		return nil
	}

	var result []selectel.Placement
	for _, item := range strings.Split(value, ",") {
		zone, flavor, _ := strings.Cut(item, ":")
		if zone == "" {
			log.Fatalf("invalid -placements item %q, want zone[:flavor]", item)
		}
		result = append(result, selectel.Placement{AvailabilityZone: zone, Flavor: flavor})
	}
	return result
}

// waitUntilRunning опрашивает Update, пока все count инстансов не станут
// Running, и пишет каждое увиденное состояние.
func waitUntilRunning(ctx context.Context, logger hclog.Logger, g *selectel.InstanceGroup, count int, pollInterval, readyTimeout time.Duration) ([]string, error) {
	deadline := time.Now().Add(readyTimeout)

	for {
		states := map[string]provider.State{}
		if err := g.Update(ctx, func(instance string, state provider.State) {
			states[instance] = state
		}); err != nil {
			logger.Error("update failed", "error", err)
		}

		var running []string
		for id, state := range states {
			logger.Info("observed instance state", "id", id, "state", state)
			if state == provider.StateRunning {
				running = append(running, id)
			}
		}
		if len(running) == count {
			return running, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s: %d of %d instances are running", readyTimeout, len(running), count)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// checkAccess реально подключается к инстансу и выполняет простую команду,
// повторяя попытки до успеха или таймаута: машина в ACTIVE ещё какое-то время
// грузится и применяет cloud-init.
func checkAccess(ctx context.Context, logger hclog.Logger, info provider.ConnectInfo, timeout, interval time.Duration, command string, useExternalAddr bool) error {
	if command == "" {
		command = accessCheckCommand(info.Protocol)
	}

	deadline := time.Now().Add(timeout)

	for {
		var stdout, stderr bytes.Buffer

		attemptCtx, cancel := context.WithTimeout(ctx, accessCheckAttemptTimeout)
		err := connector.Run(attemptCtx, info, connector.ConnectorOptions{
			RunOptions: connector.RunOptions{
				Command: command,
				Stdout:  &stdout,
				Stderr:  &stderr,
			},
			DialOptions: connector.DialOptions{UseExternalAddr: useExternalAddr},
		})
		cancel()

		if err == nil {
			logger.Info("access check succeeded", "stdout", strings.TrimSpace(stdout.String()))
			return nil
		}

		logger.Debug("access check attempt failed, retrying", "error", err, "stderr", strings.TrimSpace(stderr.String()))

		if time.Now().After(deadline) {
			return fmt.Errorf("giving up after %s: %w", timeout, err)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func accessCheckCommand(protocol provider.Protocol) string {
	switch protocol {
	case provider.ProtocolWinRM, provider.ProtocolWinRMHttps:
		return "whoami"
	default:
		return "echo smoketest-ok && uname -a"
	}
}
