package main

import (
	"gitlab.com/gitlab-org/fleeting/fleeting/plugin"

	selectel "github.com/AlexeySetevoi/selectel-fleeting-plugin"
)

func main() {
	plugin.Main(&selectel.InstanceGroup{}, selectel.Version)
}
