package main

import (
	"log"
	plugins "my-scheduler-plugins/pkg/plugins/kubecomp_mig"
	"os"

	"k8s.io/component-base/cli"
	"k8s.io/kubernetes/cmd/kube-scheduler/app"
)

func main() {
	// Register custom plugins to the scheduler framework.
	log.Printf("custom-scheduler starts!\n")
	command := app.NewSchedulerCommand(
		app.WithPlugin(plugins.Name, plugins.New),
	)

	code := cli.Run(command)
	os.Exit(code)
}
