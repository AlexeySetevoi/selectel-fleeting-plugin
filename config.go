package selectel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

const (
	metadataGroup     = "fleeting-group"
	metadataManagedBy = "managed-by"

	tagPreemptible = "preemptible"

	defaultAuthURL    = "https://cloud.api.selcloud.ru/identity/v3"
	defaultVolumeType = "universal"

	// имя сервера — <name>-<8 hex>, из него Nova делает hostname, а это
	// 63 символа
	maxNameLength = 63 - 1 - 8
)

// переменные из RC-файла Selectel; важнее конфига
const (
	envAuthURL   = "OS_AUTH_URL"
	envAccountID = "OS_USER_DOMAIN_NAME"
	envProjectID = "OS_PROJECT_ID"
	envUsername  = "OS_USERNAME"
	envPassword  = "OS_PASSWORD"
	envRegion    = "OS_REGION_NAME"
)

// имя идёт и в имя сервера, и в значение metadata
var nameRegexp = regexp.MustCompile(`^[a-z][-a-z0-9]*$`)

// UnmarshalJSON разбирает plugin_config строго. Библиотека fleeting делает
// обычный json.Unmarshal, который молча пропускает незнакомые ключи: опечатка
// в необязательном поле (preemtible, security_groups_ids) осталась бы
// незамеченной и стоила бы денег или доступа. Лучше упасть на старте.
func (g *InstanceGroup) UnmarshalJSON(data []byte) error {
	type plain InstanceGroup

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode((*plain)(g)); err != nil {
		return fmt.Errorf("invalid plugin config: %w", err)
	}
	return nil
}

// applyEnv подставляет учётные данные из окружения поверх конфига.
func (g *InstanceGroup) applyEnv() {
	for env, field := range map[string]*string{
		envAuthURL:   &g.AuthURL,
		envAccountID: &g.AccountID,
		envProjectID: &g.ProjectID,
		envUsername:  &g.Username,
		envRegion:    &g.Region,
	} {
		if value := os.Getenv(env); value != "" {
			*field = value
		}
	}
	if value := os.Getenv(envPassword); value != "" {
		g.password = value
	}
}

func (g *InstanceGroup) validate() error {
	var errs []error

	if g.settings.Protocol == "" {
		g.settings.Protocol = provider.ProtocolSSH
	}

	switch g.settings.Protocol {
	case provider.ProtocolSSH:
		if g.settings.Username == "" {
			// образы Selectel кладут ключ из key_name пользователю root
			g.settings.Username = "root"
		}
	case provider.ProtocolWinRM, provider.ProtocolWinRMHttps:
		if g.settings.Username == "" {
			g.settings.Username = "Administrator"
		}
		// пароль администратора плагин не получает
		if !g.settings.UseStaticCredentials {
			errs = append(errs, errors.New("winrm requires connector config use_static_credentials = true: the plugin does not retrieve instance passwords"))
		}
	default:
		errs = append(errs, fmt.Errorf("unsupported connector config protocol: %s", g.settings.Protocol))
	}

	if g.Name == "" {
		errs = append(errs, errors.New("missing required plugin config: name"))
	} else if !nameRegexp.MatchString(g.Name) || len(g.Name) > maxNameLength {
		errs = append(errs, fmt.Errorf("invalid plugin config name %q: must match %s and be at most %d characters", g.Name, nameRegexp, maxNameLength))
	}

	for field, value := range map[string]string{
		"account_id (or " + envAccountID + ")": g.AccountID,
		"project_id (or " + envProjectID + ")": g.ProjectID,
		"username (or " + envUsername + ")":    g.Username,
		"region (or " + envRegion + ")":        g.Region,
	} {
		if value == "" {
			errs = append(errs, fmt.Errorf("missing required plugin config: %s", field))
		}
	}
	if g.password == "" && g.PasswordFile == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: password_file (or %s)", envPassword))
	}

	custom := g.Cores != 0 || g.MemoryGB != 0
	if custom {
		if g.Cores <= 0 {
			errs = append(errs, errors.New("plugin config memory_gb requires cores"))
		}
		if g.MemoryGB <= 0 {
			errs = append(errs, errors.New("plugin config cores requires memory_gb"))
		}
		if g.MemoryGB > 0 && g.MemoryGB*1024 != math.Trunc(g.MemoryGB*1024) {
			errs = append(errs, fmt.Errorf("invalid plugin config memory_gb %v: must be a whole number of megabytes", g.MemoryGB))
		}
		if g.Flavor != "" {
			errs = append(errs, errors.New("mutually exclusive plugin config provided: flavor, cores/memory_gb"))
		}
	}

	if len(g.Placements) > 0 {
		if g.AvailabilityZone != "" {
			errs = append(errs, errors.New("mutually exclusive plugin config provided: availability_zone, placements"))
		}
		if g.Flavor != "" {
			errs = append(errs, errors.New("mutually exclusive plugin config provided: flavor, placements"))
		}
		for i, p := range g.Placements {
			if p.AvailabilityZone == "" {
				errs = append(errs, fmt.Errorf("missing required plugin config: placements[%d].availability_zone", i))
			}
			if custom && p.Flavor != "" {
				errs = append(errs, fmt.Errorf("mutually exclusive plugin config provided: placements[%d].flavor, cores/memory_gb", i))
			}
			if !custom && p.Flavor == "" {
				errs = append(errs, fmt.Errorf("missing required plugin config: placements[%d].flavor (or cores/memory_gb)", i))
			}
		}
	} else {
		if g.AvailabilityZone == "" {
			errs = append(errs, errors.New("missing required plugin config: availability_zone"))
		}
		if !custom && g.Flavor == "" {
			errs = append(errs, errors.New("missing required plugin config: flavor or cores/memory_gb"))
		}
	}

	switch g.PlacementStrategy {
	case "", strategyOrdered, strategyRoundRobin, strategyRandom:
	default:
		errs = append(errs, fmt.Errorf("invalid plugin config placement_strategy %q: must be one of %s, %s, %s", g.PlacementStrategy, strategyOrdered, strategyRoundRobin, strategyRandom))
	}

	if g.ImageID != "" && g.ImageName != "" {
		errs = append(errs, errors.New("mutually exclusive plugin config provided: image_id, image_name"))
	}
	if g.ImageID == "" && g.ImageName == "" {
		errs = append(errs, errors.New("missing required plugin config: image_id or image_name"))
	}
	if g.DiskSizeGB <= 0 {
		errs = append(errs, errors.New("missing required plugin config: disk_size_gb"))
	}

	if g.NetworkID == "" {
		errs = append(errs, errors.New("missing required plugin config: network_id"))
	}
	if g.FloatingNetworkID != "" && !g.FloatingIP {
		errs = append(errs, errors.New("plugin config floating_network_id requires floating_ip = true"))
	}

	if g.UserData != "" && g.UserDataFile != "" {
		errs = append(errs, errors.New("mutually exclusive plugin config provided: user_data, user_data_file"))
	}

	for _, key := range []string{metadataGroup, metadataManagedBy} {
		if _, ok := g.Metadata[key]; ok {
			errs = append(errs, fmt.Errorf("reserved key in plugin config metadata: %s", key))
		}
	}
	for _, tag := range g.Tags {
		if strings.EqualFold(tag, tagPreemptible) {
			errs = append(errs, fmt.Errorf("reserved plugin config tag: %s (use preemptible = true instead)", tag))
		}
	}

	return errors.Join(errs...)
}

func (g *InstanceGroup) populate() error {
	if g.password == "" {
		data, err := os.ReadFile(g.PasswordFile)
		if err != nil {
			return fmt.Errorf("failed to read password_file: %w", err)
		}
		g.password = strings.TrimSpace(string(data))
		if g.password == "" {
			return fmt.Errorf("password_file %s is empty", g.PasswordFile)
		}
	}

	if g.UserDataFile != "" {
		data, err := os.ReadFile(g.UserDataFile)
		if err != nil {
			return fmt.Errorf("failed to read user_data_file: %w", err)
		}
		g.UserData = string(data)
	}

	if g.AuthURL == "" {
		g.AuthURL = defaultAuthURL
	}
	if g.VolumeType == "" {
		g.VolumeType = defaultVolumeType
	}
	if g.PlacementStrategy == "" {
		g.PlacementStrategy = strategyOrdered
	}

	if len(g.Placements) == 0 {
		g.placements = []Placement{{AvailabilityZone: g.AvailabilityZone, Flavor: g.Flavor}}
	} else {
		g.placements = append([]Placement(nil), g.Placements...)
	}

	return nil
}

// volumeType: у Selectel тип сетевого диска привязан к зоне (universal.ru-9a),
// в конфиге задаётся без неё. Полное имя с точкой берётся как есть.
func volumeType(kind, zone string) string {
	if strings.Contains(kind, ".") {
		return kind
	}
	return kind + "." + zone
}
