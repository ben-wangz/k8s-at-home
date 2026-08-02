package main

import (
	"os"

	"github.com/ben-wangz/k8s-at-home/application/frp/client/internal/frpcctl"
)

func main() {
	os.Exit(frpcctl.Run(os.Args[1:], os.Stdout, os.Stderr))
}
