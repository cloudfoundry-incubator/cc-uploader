package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	"code.cloudfoundry.org/cc-uploader/ccclient"
	"code.cloudfoundry.org/cc-uploader/config"
	"code.cloudfoundry.org/cc-uploader/handlers"
	"code.cloudfoundry.org/cfhttp"
	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/locket"
	"github.com/cloudfoundry/dropsonde"
	"github.com/hashicorp/consul/api"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/http_server"
	"github.com/tedsuo/ifrit/sigmon"
)

var configPath = flag.String(
	"configPath",
	"",
	"path to config",
)

var uploaderConfig config.UploaderConfig

const (
	ccUploadDialTimeout         = 10 * time.Second
	ccUploadKeepAlive           = 30 * time.Second
	ccUploadTLSHandshakeTimeout = 10 * time.Second
	dropsondeOrigin             = "cc_uploader"
	communicationTimeout        = 30 * time.Second
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	flag.Parse()

	uploaderConfig, err := config.NewUploaderConfig(*configPath)
	if err != nil {
		panic(err.Error())
	}

	logger, reconfigurableSink := lagerflags.NewFromConfig("cc-uploader", uploaderConfig.LagerConfig)

	cfhttp.Initialize(communicationTimeout)

	initializeDropsonde(logger)
	consulClient, err := consuladapter.NewClientFromUrl(uploaderConfig.ConsulCluster)
	if err != nil {
		logger.Fatal("new-client-failed", err)
	}

	registrationRunner := initializeRegistrationRunner(logger, consulClient, uploaderConfig.ListenAddress, clock.NewClock())

	members := grouper.Members{
		{"cc-uploader", initializeServer(logger)},
		{"registration-runner", registrationRunner},
	}

	if uploaderConfig.DebugServerConfig.DebugAddress != "" {
		members = append(grouper.Members{
			{"debug-server", debugserver.Runner(uploaderConfig.DebugServerConfig.DebugAddress, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)

	monitor := ifrit.Invoke(sigmon.New(group))
	logger.Info("ready")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func initializeDropsonde(logger lager.Logger) {
	dropsondeDestination := fmt.Sprint("localhost:", uploaderConfig.DropsondePort)
	err := dropsonde.Initialize(dropsondeDestination, dropsondeOrigin)
	if err != nil {
		logger.Error("failed to initialize dropsonde: %v", err)
	}
}

func initializeServer(logger lager.Logger) ifrit.Runner {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial: (&net.Dialer{
			Timeout:   ccUploadDialTimeout,
			KeepAlive: ccUploadKeepAlive,
		}).Dial,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: uploaderConfig.SkipCertVerify,
		},
		TLSHandshakeTimeout: ccUploadTLSHandshakeTimeout,
	}

	pollerHttpClient := cfhttp.NewClient()
	pollerHttpClient.Transport = transport

	uploader := ccclient.NewUploader(logger, &http.Client{Transport: transport})
	poller := ccclient.NewPoller(logger, pollerHttpClient, time.Duration(uploaderConfig.CCJobPollingInterval))

	ccUploaderHandler, err := handlers.New(uploader, poller, logger)
	if err != nil {
		logger.Error("router-building-failed", err)
		os.Exit(1)
	}

	return http_server.New(uploaderConfig.ListenAddress, ccUploaderHandler)
}

func initializeRegistrationRunner(logger lager.Logger, consulClient consuladapter.Client, listenAddress string, clock clock.Clock) ifrit.Runner {
	_, portString, err := net.SplitHostPort(listenAddress)
	if err != nil {
		logger.Fatal("failed-invalid-listen-address", err)
	}
	portNum, err := net.LookupPort("tcp", portString)
	if err != nil {
		logger.Fatal("failed-invalid-listen-port", err)
	}

	registration := &api.AgentServiceRegistration{
		Name: "cc-uploader",
		Port: portNum,
		Check: &api.AgentServiceCheck{
			TTL: "3s",
		},
	}

	return locket.NewRegistrationRunner(logger, registration, consulClient, locket.RetryInterval, clock)
}
