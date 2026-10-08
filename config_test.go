package selectel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func TestValidate(t *testing.T) {
	winrm := provider.Settings{ConnectorConfig: provider.ConnectorConfig{Protocol: provider.ProtocolWinRM}}

	tests := []struct {
		name     string
		mutate   func(g *InstanceGroup)
		settings provider.Settings
		wantErr  string // пусто — ошибки быть не должно
	}{
		{name: "valid"},
		{name: "no name", mutate: func(g *InstanceGroup) { g.Name = "" }, wantErr: "plugin config: name"},
		{name: "bad name", mutate: func(g *InstanceGroup) { g.Name = "Runner_1" }, wantErr: "invalid plugin config name"},
		{name: "long name", mutate: func(g *InstanceGroup) { g.Name = strings.Repeat("a", maxNameLength+1) }, wantErr: "invalid plugin config name"},
		{name: "no account", mutate: func(g *InstanceGroup) { g.AccountID = "" }, wantErr: "account_id (or OS_USER_DOMAIN_NAME)"},
		{name: "no project", mutate: func(g *InstanceGroup) { g.ProjectID = "" }, wantErr: "project_id (or OS_PROJECT_ID)"},
		{name: "no username", mutate: func(g *InstanceGroup) { g.Username = "" }, wantErr: "username (or OS_USERNAME)"},
		{name: "no region", mutate: func(g *InstanceGroup) { g.Region = "" }, wantErr: "region (or OS_REGION_NAME)"},
		{name: "no password", mutate: func(g *InstanceGroup) { g.password = "" }, wantErr: "password_file (or OS_PASSWORD)"},
		{name: "password file", mutate: func(g *InstanceGroup) { g.password, g.PasswordFile = "", "/etc/gitlab-runner/selectel-password" }},
		{name: "no zone", mutate: func(g *InstanceGroup) { g.AvailabilityZone = "" }, wantErr: "plugin config: availability_zone"},
		{name: "no flavor", mutate: func(g *InstanceGroup) { g.Flavor = "" }, wantErr: "flavor or cores/memory_gb"},
		{name: "custom flavor", mutate: func(g *InstanceGroup) { g.Flavor, g.Cores, g.MemoryGB = "", 2, 4 }},
		{name: "flavor and cores", mutate: func(g *InstanceGroup) { g.Cores, g.MemoryGB = 2, 4 }, wantErr: "flavor, cores/memory_gb"},
		{name: "cores without memory", mutate: func(g *InstanceGroup) { g.Flavor, g.Cores = "", 2 }, wantErr: "cores requires memory_gb"},
		{name: "memory without cores", mutate: func(g *InstanceGroup) { g.Flavor, g.MemoryGB = "", 2 }, wantErr: "memory_gb requires cores"},
		{name: "fractional megabytes", mutate: func(g *InstanceGroup) { g.Flavor, g.Cores, g.MemoryGB = "", 2, 1.0001 }, wantErr: "whole number of megabytes"},
		{name: "no disk size", mutate: func(g *InstanceGroup) { g.DiskSizeGB = 0 }, wantErr: "disk_size_gb"},
		{name: "no image", mutate: func(g *InstanceGroup) { g.ImageID = "" }, wantErr: "image_id or image_name"},
		{name: "both images", mutate: func(g *InstanceGroup) { g.ImageName = "Ubuntu 24.04 LTS 64-bit" }, wantErr: "image_id, image_name"},
		{name: "no network", mutate: func(g *InstanceGroup) { g.NetworkID = "" }, wantErr: "network_id"},
		{name: "floating network without floating ip", mutate: func(g *InstanceGroup) { g.FloatingNetworkID = "ext" }, wantErr: "floating_network_id requires floating_ip"},
		{
			name:    "placements with zone",
			mutate:  func(g *InstanceGroup) { g.Placements = []Placement{{AvailabilityZone: "ru-9b", Flavor: "SL1.2-4096"}} },
			wantErr: "availability_zone, placements",
		},
		{
			name: "placements with flavor",
			mutate: func(g *InstanceGroup) {
				g.AvailabilityZone = ""
				g.Placements = []Placement{{AvailabilityZone: "ru-9b", Flavor: "SL1.2-4096"}}
			},
			wantErr: "flavor, placements",
		},
		{
			name: "placements only",
			mutate: func(g *InstanceGroup) {
				g.AvailabilityZone, g.Flavor = "", ""
				g.Placements = []Placement{{AvailabilityZone: "ru-9b", Flavor: "SL1.2-4096"}}
			},
		},
		{
			name: "placement without zone",
			mutate: func(g *InstanceGroup) {
				g.AvailabilityZone, g.Flavor = "", ""
				g.Placements = []Placement{{Flavor: "SL1.2-4096"}}
			},
			wantErr: "placements[0].availability_zone",
		},
		{
			name: "placement without flavor",
			mutate: func(g *InstanceGroup) {
				g.AvailabilityZone, g.Flavor = "", ""
				g.Placements = []Placement{{AvailabilityZone: "ru-9b"}}
			},
			wantErr: "placements[0].flavor",
		},
		{
			name: "placements with custom flavor",
			mutate: func(g *InstanceGroup) {
				g.AvailabilityZone, g.Flavor, g.Cores, g.MemoryGB = "", "", 2, 4
				g.Placements = []Placement{{AvailabilityZone: "ru-9a"}, {AvailabilityZone: "ru-9b"}}
			},
		},
		{
			name: "placement flavor with custom flavor",
			mutate: func(g *InstanceGroup) {
				g.AvailabilityZone, g.Flavor, g.Cores, g.MemoryGB = "", "", 2, 4
				g.Placements = []Placement{{AvailabilityZone: "ru-9a", Flavor: "SL1.2-4096"}}
			},
			wantErr: "placements[0].flavor, cores/memory_gb",
		},
		{
			name:    "bad placement strategy",
			mutate:  func(g *InstanceGroup) { g.PlacementStrategy = "round-robin" },
			wantErr: "placement_strategy",
		},
		{name: "round robin", mutate: func(g *InstanceGroup) { g.PlacementStrategy = strategyRoundRobin }},
		{
			name:    "both user data",
			mutate:  func(g *InstanceGroup) { g.UserData, g.UserDataFile = "x", "/tmp/x" },
			wantErr: "user_data, user_data_file",
		},
		{
			name:    "reserved metadata",
			mutate:  func(g *InstanceGroup) { g.Metadata = map[string]string{metadataGroup: "x"} },
			wantErr: "reserved key in plugin config metadata",
		},
		{
			name:    "reserved tag",
			mutate:  func(g *InstanceGroup) { g.Tags = []string{"Preemptible"} },
			wantErr: "use preemptible = true",
		},
		{name: "winrm without static credentials", settings: winrm, wantErr: "use_static_credentials"},
		{
			name: "winrm with static credentials",
			settings: provider.Settings{ConnectorConfig: provider.ConnectorConfig{
				Protocol:             provider.ProtocolWinRMHttps,
				UseStaticCredentials: true,
			}},
		},
		{
			name:     "unknown protocol",
			settings: provider.Settings{ConnectorConfig: provider.ConnectorConfig{Protocol: "telnet"}},
			wantErr:  "unsupported connector config protocol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := validGroup()
			g.settings = tt.settings
			if tt.mutate != nil {
				tt.mutate(g)
			}

			err := g.validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("validate() = nil, want error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	g := &InstanceGroup{}
	err := g.validate()
	if err == nil {
		t.Fatal("validate() = nil for an empty config")
	}
	for _, want := range []string{"name", "account_id", "project_id", "username", "region", "password_file",
		"availability_zone", "flavor", "disk_size_gb", "image_id", "network_id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validate() error does not mention %s: %v", want, err)
		}
	}
}

func TestPopulateDefaults(t *testing.T) {
	g := validGroup()
	if err := g.populate(); err != nil {
		t.Fatalf("populate() = %v", err)
	}

	if g.AuthURL != defaultAuthURL || g.VolumeType != defaultVolumeType {
		t.Fatalf("defaults: auth_url=%q volume_type=%q", g.AuthURL, g.VolumeType)
	}
	want := Placement{AvailabilityZone: "ru-9a", Flavor: "SL1.2-4096"}
	if len(g.placements) != 1 || g.placements[0] != want {
		t.Fatalf("placements = %+v, want [%+v]", g.placements, want)
	}
}

func TestPopulateUserDataFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "user-data.yml")
	if err := os.WriteFile(file, []byte("#cloud-config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := validGroup()
	g.UserDataFile = file
	if err := g.populate(); err != nil {
		t.Fatalf("populate() = %v", err)
	}
	if g.UserData != "#cloud-config\n" {
		t.Fatalf("UserData = %q", g.UserData)
	}

	g = validGroup()
	g.UserDataFile = filepath.Join(t.TempDir(), "missing.yml")
	if err := g.populate(); err == nil {
		t.Fatal("populate() = nil for a missing user_data_file")
	}
}

func TestPopulateEmptyPasswordFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(file, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g := validGroup()
	g.password, g.PasswordFile = "", file
	if err := g.populate(); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("populate() = %v, want an empty password_file error", err)
	}
}

func TestVolumeType(t *testing.T) {
	for _, tt := range []struct{ kind, zone, want string }{
		{"universal", "ru-9a", "universal.ru-9a"},
		{"fast", "ru-7b", "fast.ru-7b"},
		{"fast.ru-9a", "ru-9b", "fast.ru-9a"},
	} {
		if got := volumeType(tt.kind, tt.zone); got != tt.want {
			t.Errorf("volumeType(%q, %q) = %q, want %q", tt.kind, tt.zone, got, tt.want)
		}
	}
}
