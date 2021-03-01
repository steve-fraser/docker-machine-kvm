package main

import (
	"github.com/steve-fraser/docker-machine-kvm"
	"github.com/rancher/machine/libmachine/drivers/plugin"
)

func main() {
	plugin.RegisterDriver(kvm.NewDriver("default", "path"))
}
