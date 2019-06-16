/*
Copyright 2018 The Kubernetes Authors.

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
	"flag"
	"k8s.io/client-go/tools/clientcmd"
	"log"
	"os"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/cloud/openstack"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/apis"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/controller"
	"sigs.k8s.io/cluster-api-provider-openstack/pkg/record"
	clusterapis "sigs.k8s.io/cluster-api/pkg/apis"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

var logFlushFreq = flag.Duration("log-flush-frequency", 5*time.Second, "Maximum number of seconds between log flushes")

var userDataControlPlane = flag.String("user-data-control-plane", "", "if set, the given file is used as user data for the control plane nodes")
var userDataWorker = flag.String("user-data-worker", "", "if set, the given file is used as user data for the worker nodes")
var userDataPostprocessor = flag.String("user-data-postprocessor", "", "postprocessor to user for the user data")

func initLogs() {

	flag.Set("alsologtostderr", "true")
	flag.Parse()

	// The default klog flush interval is 30 seconds, which is frighteningly long.
	go wait.Until(klog.Flush, *logFlushFreq, wait.NeverStop)

	klogFlags := flag.NewFlagSet("klog", flag.ExitOnError)
	klog.InitFlags(klogFlags)

	// Sync the glog and klog flags.
	flag.CommandLine.VisitAll(func(f1 *flag.Flag) {
		f2 := klogFlags.Lookup(f1.Name)
		if f2 != nil {
			value := f1.Value.String()
			f2.Value.Set(value)
		}
	})
	vFlag := klogFlags.Lookup("v")
	vFlag.Value.Set("4")

	logtostderrFlag := klogFlags.Lookup("logtostderr")
	logtostderrFlag.Value.Set("true")
}

func main() {

	initLogs()

	// Get a config to talk to the apiserver
	// TODO upstream feature to specify context in sigs.k8s.io/controller-runtime/pkg/client/config/config.go
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: os.Getenv("KUBECONFIG")},
		&clientcmd.ConfigOverrides{CurrentContext: os.Getenv("KUBECONTEXT")}).ClientConfig()

	//cfg, err := config.GetConfig()
	if err != nil {
		klog.Fatal(err)
	}

	// Create a new Cmd to provide shared dependencies and start components
	mgr, err := manager.New(cfg, manager.Options{})
	if err != nil {
		klog.Fatal(err)
	}

	record.InitFromRecorder(mgr.GetRecorder("openstack-controller"))
	klog.Infof("Initializing Dependencies.")

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	if err := clusterapis.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	controller.SetConfig(openstack.ActuatorConfig{
		UserDataControlPlane:  *userDataControlPlane,
		UserDataWorker:        *userDataWorker,
		UserDataPostprocessor: *userDataPostprocessor,
	})

	// Setup all Controllers
	if err := controller.AddToManager(mgr); err != nil {
		klog.Fatal(err)
	}

	log.Printf("Starting the Cmd.")

	// Start the Cmd
	log.Fatal(mgr.Start(signals.SetupSignalHandler()))
}
