//
// DISCLAIMER
//
// Copyright 2018 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package main

import (
	goflag "flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	"github.com/arangodb/kube-arangodb/pkg/client"
	"github.com/arangodb/kube-arangodb/pkg/logging"
	"github.com/arangodb/kube-arangodb/pkg/operator"
	"github.com/arangodb/kube-arangodb/pkg/util/constants"
	"github.com/arangodb/kube-arangodb/pkg/util/k8sutil"
	"github.com/arangodb/kube-arangodb/pkg/util/probe"
	"github.com/arangodb/kube-arangodb/pkg/util/retry"
)

const (
	defaultServerHost = "0.0.0.0"
	defaultServerPort = 8528
	defaultLogLevel   = "debug"
)

var (
	projectVersion = "dev"
	projectBuild   = "dev"

	maskAny = errors.WithStack

	cmdMain = cobra.Command{
		Use: "arangodb_operator",
		Run: cmdMainRun,
	}

	logLevel   string
	cliLog     = logging.NewRootLogger()
	logService logging.Service
	server     struct {
		host string
		port int
	}
	operatorOptions struct {
		enableDeployment bool // Run deployment operator
		enableStorage    bool // Run deployment operator
	}
	chaosOptions struct {
		allowed bool
	}
	deploymentProbe probe.Probe
	storageProbe    probe.Probe
)

func init() {
	f := cmdMain.Flags()
	f.StringVar(&server.host, "server.host", defaultServerHost, "Host to listen on")
	f.IntVar(&server.port, "server.port", defaultServerPort, "Port to listen on")
	f.StringVar(&logLevel, "log.level", defaultLogLevel, "Set initial log level")
	f.BoolVar(&operatorOptions.enableDeployment, "operator.deployment", false, "Enable to run the ArangoDeployment operator")
	f.BoolVar(&operatorOptions.enableStorage, "operator.storage", false, "Enable to run the ArangoLocalStorage operator")
	f.BoolVar(&chaosOptions.allowed, "chaos.allowed", false, "Set to allow chaos in deployments. Only activated when allowed and enabled in deployment")
}

func main() {
	flag.CommandLine.AddGoFlagSet(goflag.CommandLine)
	cmdMain.Execute()
}

// Show usage
func cmdUsage(cmd *cobra.Command, args []string) {
	cmd.Usage()
}

// Run the operator
func cmdMainRun(cmd *cobra.Command, args []string) {
	goflag.CommandLine.Parse([]string{"-logtostderr"})
	var err error
	logService, err = logging.NewService(logLevel)
	if err != nil {
		cliLog.Fatal().Err(err).Msg("Failed to initialize log service")
	}

	// Check operating mode
	if !operatorOptions.enableDeployment && !operatorOptions.enableStorage {
		cliLog.Fatal().Err(err).Msg("Turn on --operator.deployment or --operator.storage or both")
	}

	// Log version
	cliLog.Info().Msgf("Starting arangodb-operator, version %s build %s", projectVersion, projectBuild)

	// Get environment
	namespace := os.Getenv(constants.EnvOperatorPodNamespace)
	if len(namespace) == 0 {
		cliLog.Fatal().Msgf("%s environment variable missing", constants.EnvOperatorPodNamespace)
	}
	name := os.Getenv(constants.EnvOperatorPodName)
	if len(name) == 0 {
		cliLog.Fatal().Msgf("%s environment variable missing", constants.EnvOperatorPodName)
	}

	// Get host name
	id, err := os.Hostname()
	if err != nil {
		cliLog.Fatal().Err(err).Msg("Failed to get hostname")
	}

	http.HandleFunc("/health", probe.LivenessHandler)
	http.HandleFunc("/ready/deployment", deploymentProbe.ReadyHandler)
	http.HandleFunc("/ready/storage", storageProbe.ReadyHandler)
	http.Handle("/metrics", prometheus.Handler())
	listenAddr := net.JoinHostPort(server.host, strconv.Itoa(server.port))
	go http.ListenAndServe(listenAddr, nil)

	cfg, deps, err := newOperatorConfigAndDeps(id+"-"+name, namespace, name)
	if err != nil {
		cliLog.Fatal().Err(err).Msg("Failed to create operator config & deps")
	}

	//	startChaos(context.Background(), cfg.KubeCli, cfg.Namespace, chaosLevel)

	o, err := operator.NewOperator(cfg, deps)
	if err != nil {
		cliLog.Fatal().Err(err).Msg("Failed to create operator")
	}
	o.Run()
}

// newOperatorConfigAndDeps creates operator config & dependencies.
func newOperatorConfigAndDeps(id, namespace, name string) (operator.Config, operator.Dependencies, error) {
	kubecli, err := k8sutil.NewKubeClient()
	if err != nil {
		return operator.Config{}, operator.Dependencies{}, maskAny(err)
	}

	serviceAccount, err := getMyPodServiceAccount(kubecli, namespace, name)
	if err != nil {
		return operator.Config{}, operator.Dependencies{}, maskAny(fmt.Errorf("Failed to get my pod's service account: %s", err))
	}

	kubeExtCli, err := k8sutil.NewKubeExtClient()
	if err != nil {
		return operator.Config{}, operator.Dependencies{}, maskAny(fmt.Errorf("Failed to create k8b api extensions client: %s", err))
	}
	crCli, err := client.NewInCluster()
	if err != nil {
		return operator.Config{}, operator.Dependencies{}, maskAny(fmt.Errorf("Failed to created versioned client: %s", err))
	}
	eventRecorder := createRecorder(cliLog, kubecli, name, namespace)

	cfg := operator.Config{
		ID:               id,
		Namespace:        namespace,
		PodName:          name,
		ServiceAccount:   serviceAccount,
		EnableDeployment: operatorOptions.enableDeployment,
		EnableStorage:    operatorOptions.enableStorage,
		AllowChaos:       chaosOptions.allowed,
	}
	deps := operator.Dependencies{
		LogService:      logService,
		KubeCli:         kubecli,
		KubeExtCli:      kubeExtCli,
		CRCli:           crCli,
		EventRecorder:   eventRecorder,
		DeploymentProbe: &deploymentProbe,
		StorageProbe:    &storageProbe,
	}

	return cfg, deps, nil
}

// getMyPodServiceAccount looks up the service account of the pod with given name in given namespace
func getMyPodServiceAccount(kubecli kubernetes.Interface, namespace, name string) (string, error) {
	var sa string
	op := func() error {
		pod, err := kubecli.CoreV1().Pods(namespace).Get(name, metav1.GetOptions{})
		if err != nil {
			cliLog.Error().
				Err(err).
				Str("name", name).
				Msg("Failed to get operator pod")
			return maskAny(err)
		}
		sa = pod.Spec.ServiceAccountName
		return nil
	}
	if err := retry.Retry(op, time.Minute*5); err != nil {
		return "", maskAny(err)
	}
	return sa, nil
}

func createRecorder(log zerolog.Logger, kubecli kubernetes.Interface, name, namespace string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(func(format string, args ...interface{}) {
		log.Info().Msgf(format, args...)
	})
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubecli.Core().RESTClient()).Events(namespace)})
	return eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: name})
}
