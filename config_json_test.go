package selectel

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// decodeLikeRunner повторяет путь конфига в бою: gitlab-runner читает
// plugin_config из TOML в map, отдаёт плагину как JSON, а fleeting делает
// json.Unmarshal в InstanceGroup.
func decodeLikeRunner(t *testing.T, pluginConfig map[string]any) (*InstanceGroup, error) {
	t.Helper()

	data, err := json.Marshal(pluginConfig)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}

	g := &InstanceGroup{}
	return g, json.Unmarshal(data, g)
}

func TestConfigFromTOML(t *testing.T) {
	var doc map[string]any
	_, err := toml.Decode(`
name                = "ci-linux"
auth_url            = "https://cloud.api.selcloud.ru/identity/v3"
account_id          = "123456"
project_id          = "0123456789abcdef0123456789abcdef"
username            = "gitlab-fleeting"
password_file       = "/etc/gitlab-runner/selectel-password"
region              = "ru-9"
cores               = 4
memory_gb           = 0.5
image_name          = "Ubuntu 24.04 LTS 64-bit"
volume_type         = "fast"
disk_size_gb        = 93
network_id          = "net-1"
subnet_id           = "subnet-1"
security_group_ids  = ["sg-1", "sg-2"]
floating_ip         = true
floating_network_id = "ext-net"
preemptible         = true
tags                = ["ci"]
user_data           = "#cloud-config\n"
placements = [
  { availability_zone = "ru-9a" },
  { availability_zone = "ru-9b" },
]

[metadata]
team = "platform"
`, &doc)
	if err != nil {
		t.Fatalf("toml.Decode() = %v", err)
	}

	got, err := decodeLikeRunner(t, doc)
	if err != nil {
		t.Fatalf("decoding plugin config = %v", err)
	}

	want := &InstanceGroup{ //nolint:gosec // путь к файлу с паролем, не пароль
		Name:              "ci-linux",
		AuthURL:           "https://cloud.api.selcloud.ru/identity/v3",
		AccountID:         "123456",
		ProjectID:         "0123456789abcdef0123456789abcdef",
		Username:          "gitlab-fleeting",
		PasswordFile:      "/etc/gitlab-runner/selectel-password",
		Region:            "ru-9",
		Placements:        []Placement{{AvailabilityZone: "ru-9a"}, {AvailabilityZone: "ru-9b"}},
		Cores:             4,
		MemoryGB:          0.5,
		ImageName:         "Ubuntu 24.04 LTS 64-bit",
		VolumeType:        "fast",
		DiskSizeGB:        93,
		NetworkID:         "net-1",
		SubnetID:          "subnet-1",
		SecurityGroupIDs:  []string{"sg-1", "sg-2"},
		FloatingIP:        true,
		FloatingNetworkID: "ext-net",
		Preemptible:       true,
		Tags:              []string{"ci"},
		Metadata:          map[string]string{"team": "platform"},
		UserData:          "#cloud-config\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded config:\n got %+v\nwant %+v", got, want)
	}

	if err := got.validate(); err != nil {
		t.Fatalf("validate() = %v", err)
	}
}

// Каждое экспортируемое поле должно читаться из конфига под своим ключом: поле
// без json-тега читалось бы под именем Go, а не так, как написано в README.
func TestConfigFieldsHaveJSONTags(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(InstanceGroup{}), reflect.TypeOf(Placement{})} {
		for i := range typ.NumField() {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			tag := field.Tag.Get("json")
			if tag == "" || tag == "-" || tag != strings.ToLower(tag) {
				t.Errorf("%s.%s: json tag %q, want a snake_case key", typ.Name(), field.Name, tag)
			}
		}
	}
}

func TestConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		wantErr string
	}{
		{
			name:    "typo in an optional field",
			config:  map[string]any{"name": "ci", "preemtible": true},
			wantErr: `unknown field "preemtible"`,
		},
		{
			name:    "typo inside placements",
			config:  map[string]any{"placements": []any{map[string]any{"availability_zone": "ru-9a", "flavour": "SL1.2-4096"}}},
			wantErr: `unknown field "flavour"`,
		},
		{
			name:    "string instead of a number",
			config:  map[string]any{"cores": "4"},
			wantErr: "cores",
		},
		{
			name:    "fractional cores",
			config:  map[string]any{"cores": 2.5},
			wantErr: "cores",
		},
		{
			name:    "scalar instead of a list",
			config:  map[string]any{"security_group_ids": "sg-1"},
			wantErr: "security_group_ids",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeLikeRunner(t, tt.config)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decoding = %v, want an error mentioning %s", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), "invalid plugin config") {
				t.Fatalf("decoding = %v, want it to say the plugin config is at fault", err)
			}
		})
	}
}

// Целое memory_gb из TOML (8, а не 8.0) должно читаться в float-поле.
func TestConfigIntegerMemory(t *testing.T) {
	var doc map[string]any
	if _, err := toml.Decode("memory_gb = 8", &doc); err != nil {
		t.Fatal(err)
	}

	g, err := decodeLikeRunner(t, doc)
	if err != nil || g.MemoryGB != 8 {
		t.Fatalf("memory_gb = %v, %v", g.MemoryGB, err)
	}
}

var tomlBlock = regexp.MustCompile("(?s)```toml\n(.*?)```")

// Примеры конфигов из README должны разбираться плагином и проходить
// валидацию — иначе документация разойдётся с кодом незаметно.
func TestREADMEExamples(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}

	full := 0

	for i, match := range tomlBlock.FindAllStringSubmatch(string(readme), -1) {
		var doc map[string]any
		if _, err := toml.Decode(match[1], &doc); err != nil {
			t.Errorf("README toml block #%d does not parse: %v", i+1, err)
			continue
		}

		pluginConfig, ok := findTable(doc, "plugin_config")
		if !ok {
			continue // блок про policy, а не про плагин
		}

		g, err := decodeLikeRunner(t, pluginConfig)
		if err != nil {
			t.Errorf("README toml block #%d: %v", i+1, err)
			continue
		}

		if g.ProjectID == "" {
			continue // фрагмент (например, только placements), валидировать нечего
		}
		full++

		if connector, ok := findTable(doc, "connector_config"); ok {
			if protocol, ok := connector["protocol"].(string); ok {
				g.settings.Protocol = provider.Protocol(protocol)
			}
			g.settings.UseStaticCredentials, _ = connector["use_static_credentials"].(bool)
		}

		if err := g.validate(); err != nil {
			t.Errorf("README toml block #%d (name=%s) is not a valid config: %v", i+1, g.Name, err)
		}
	}

	if full < 2 {
		t.Fatalf("found %d full plugin_config examples in README, want at least the Linux and Windows ones", full)
	}
}

// findTable ищет таблицу по имени на любой глубине: в README она лежит то под
// [[runners]], то отдельным фрагментом.
func findTable(node any, name string) (map[string]any, bool) {
	switch v := node.(type) {
	case map[string]any:
		if table, ok := v[name].(map[string]any); ok {
			return table, true
		}
		for _, child := range v {
			if table, ok := findTable(child, name); ok {
				return table, true
			}
		}
	case []map[string]any:
		for _, child := range v {
			if table, ok := findTable(child, name); ok {
				return table, true
			}
		}
	case []any:
		for _, child := range v {
			if table, ok := findTable(child, name); ok {
				return table, true
			}
		}
	}
	return nil, false
}
