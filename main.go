/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"github.com/google/cadvisor/container"
	"github.com/google/cadvisor/container/docker"
	"math/rand"
	"os"
	"time"

	"github.com/Mirantis/cri-dockerd/pkg/app"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/logs"

	_ "github.com/google/cadvisor/container/docker/install"
)

func main() {
	rand.Seed(time.Now().UnixNano())

	err := container.RegisterPlugin("docker", docker.NewPlugin())
	if err != nil {
		fmt.Println("kinara: RegisterPlugin", "docker")
	}
	command := app.NewDockerCRICommand(server.SetupSignalHandler())
	logs.InitLogs()
	defer logs.FlushLogs()

	if err := command.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}
