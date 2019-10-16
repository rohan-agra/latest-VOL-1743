/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

//Package main invokes the application
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/opencord/voltha-go/adapters"
	"github.com/opencord/voltha-go/adapters/adapterif"
	com "github.com/opencord/voltha-go/adapters/common"
	"github.com/opencord/voltha-go/common/log"
	"github.com/opencord/voltha-go/common/probe"
	"github.com/opencord/voltha-go/db/kvstore"
	"github.com/opencord/voltha-go/kafka"
	ac "github.com/opencord/voltha-openolt-adapter/adaptercore"
	"github.com/opencord/voltha-openolt-adapter/config"
	"github.com/opencord/voltha-openolt-adapter/config/version"
	ic "github.com/opencord/voltha-protos/go/inter_container"
	"github.com/opencord/voltha-protos/go/voltha"
)

type adapter struct {
	instanceID       string
	config           *config.AdapterFlags
	iAdapter         adapters.IAdapter
	kafkaClient      kafka.Client
	kvClient         kvstore.Client
	kip              *kafka.InterContainerProxy
	coreProxy        adapterif.CoreProxy
	adapterProxy     adapterif.AdapterProxy
	eventProxy       adapterif.EventProxy
	halted           bool
	exitChannel      chan int
	receiverChannels []<-chan *ic.InterContainerMessage
}

func init() {
	_, _ = log.AddPackage(log.JSON, log.DebugLevel, nil)
}

func newAdapter(cf *config.AdapterFlags) *adapter {
	var a adapter
	a.instanceID = cf.InstanceID
	a.config = cf
	a.halted = false
	a.exitChannel = make(chan int, 1)
	a.receiverChannels = make([]<-chan *ic.InterContainerMessage, 0)
	return &a
}

func (a *adapter) start(ctx context.Context) {

	var p *probe.Probe
	if value := ctx.Value(probe.ProbeContextKey); value != nil {
		if _, ok := value.(*probe.Probe); ok {
			p = value.(*probe.Probe)
			p.RegisterService("message-bus",
				"kv-store",
				"core-request-handler",
				"register-with-core",
				"inter-container-proxy")
		}
	}

	log.Info("Starting Core Adapter components")
	var err error

	// Setup KV Client
	log.Debugw("create-kv-client", log.Fields{"kvstore": a.config.KVStoreType})
	if err = a.setKVClient(); err != nil {
		log.Fatal("error-setting-kv-client")
	}

	if p != nil {
		p.UpdateStatus("kv-store", probe.ServiceStatusRunning)
	}

	// Setup Kafka Client
	if a.kafkaClient, err = newKafkaClient("sarama", a.config.KafkaAdapterHost, a.config.KafkaAdapterPort); err != nil {
		log.Fatal("Unsupported-common-client")
	}

	if p != nil {
		p.UpdateStatus("message-bus", probe.ServiceStatusRunning)
	}

	// Start the common InterContainer Proxy - retries indefinitely
	if a.kip, err = a.startInterContainerProxy(-1); err != nil {
		log.Fatal("error-starting-inter-container-proxy")
	}

	if p != nil {
		p.UpdateStatus("inter-container-proxy", probe.ServiceStatusRunning)
	}

	// Create the core proxy to handle requests to the Core
	a.coreProxy = com.NewCoreProxy(a.kip, a.config.Topic, a.config.CoreTopic)

	// Create the adaptor proxy to handle request between olt and onu
	a.adapterProxy = com.NewAdapterProxy(a.kip, "brcm_openomci_onu", a.config.CoreTopic)

	// Create the event proxy to post events to KAFKA
	a.eventProxy = com.NewEventProxy(com.MsgClient(a.kafkaClient), com.MsgTopic(kafka.Topic{Name: a.config.EventTopic}))

	// Create the open OLT adapter
	if a.iAdapter, err = a.startOpenOLT(ctx, a.kip, a.coreProxy, a.adapterProxy, a.eventProxy,
		a.config.OnuNumber,
		a.config.KVStoreHost, a.config.KVStorePort, a.config.KVStoreType); err != nil {
		log.Fatal("error-starting-inter-container-proxy")
	}

	// Register the core request handler
	if err = a.setupRequestHandler(ctx, a.instanceID, a.iAdapter); err != nil {
		log.Fatal("error-setting-core-request-handler")
	}

	// Register this adapter to the Core - retries indefinitely
	if err = a.registerWithCore(-1); err != nil {
		log.Fatal("error-registering-with-core")
	}
	if p != nil {
		p.UpdateStatus("register-with-core", probe.ServiceStatusRunning)
	}
}

func (a *adapter) stop(ctx context.Context) {
	// Stop leadership tracking
	a.halted = true

	// send exit signal
	a.exitChannel <- 0

	// Cleanup - applies only if we had a kvClient
	if a.kvClient != nil {
		// Release all reservations
		if err := a.kvClient.ReleaseAllReservations(); err != nil {
			log.Infow("fail-to-release-all-reservations", log.Fields{"error": err})
		}
		// Close the DB connection
		a.kvClient.Close()
	}

	// TODO:  More cleanup
}

func newKVClient(storeType, address string, timeout int) (kvstore.Client, error) {

	log.Infow("kv-store-type", log.Fields{"store": storeType})
	switch storeType {
	case "consul":
		return kvstore.NewConsulClient(address, timeout)
	case "etcd":
		return kvstore.NewEtcdClient(address, timeout)
	}
	return nil, errors.New("unsupported-kv-store")
}

func newKafkaClient(clientType, host string, port int) (kafka.Client, error) {

	log.Infow("common-client-type", log.Fields{"client": clientType})
	switch clientType {
	case "sarama":
		return kafka.NewSaramaClient(
			kafka.Host(host),
			kafka.Port(port),
			kafka.ProducerReturnOnErrors(true),
			kafka.ProducerReturnOnSuccess(true),
			kafka.ProducerMaxRetries(6),
			kafka.ProducerRetryBackoff(time.Millisecond*30),
			kafka.MetadatMaxRetries(15)), nil
	}

	return nil, errors.New("unsupported-client-type")
}

func (a *adapter) setKVClient() error {
	addr := a.config.KVStoreHost + ":" + strconv.Itoa(a.config.KVStorePort)
	client, err := newKVClient(a.config.KVStoreType, addr, a.config.KVStoreTimeout)
	if err != nil {
		a.kvClient = nil
		log.Error(err)
		return err
	}
	a.kvClient = client
	return nil
}

func (a *adapter) startInterContainerProxy(retries int) (*kafka.InterContainerProxy, error) {
	log.Infow("starting-intercontainer-messaging-proxy", log.Fields{"host": a.config.KafkaAdapterHost,
		"port": a.config.KafkaAdapterPort, "topic": a.config.Topic})
	var err error
	var kip *kafka.InterContainerProxy
	if kip, err = kafka.NewInterContainerProxy(
		kafka.InterContainerHost(a.config.KafkaAdapterHost),
		kafka.InterContainerPort(a.config.KafkaAdapterPort),
		kafka.MsgClient(a.kafkaClient),
		kafka.DefaultTopic(&kafka.Topic{Name: a.config.Topic})); err != nil {
		log.Errorw("fail-to-create-common-proxy", log.Fields{"error": err})
		return nil, err
	}
	count := 0
	for {
		if err = kip.Start(); err != nil {
			log.Warnw("error-starting-messaging-proxy", log.Fields{"error": err})
			if retries == count {
				return nil, err
			}
			count = +1
			// Take a nap before retrying
			time.Sleep(2 * time.Second)
		} else {
			break
		}
	}
	log.Info("common-messaging-proxy-created")
	return kip, nil
}

func (a *adapter) startOpenOLT(ctx context.Context, kip *kafka.InterContainerProxy,
	cp adapterif.CoreProxy, ap adapterif.AdapterProxy, ep adapterif.EventProxy, onuNumber int, kvStoreHost string,
	kvStorePort int, KVStoreType string) (*ac.OpenOLT, error) {
	log.Info("starting-open-olt")
	var err error
	sOLT := ac.NewOpenOLT(ctx, a.kip, cp, ap, ep, onuNumber, kvStoreHost, kvStorePort, KVStoreType)

	if err = sOLT.Start(ctx); err != nil {
		log.Fatalw("error-starting-messaging-proxy", log.Fields{"error": err})
		return nil, err
	}

	log.Info("open-olt-started")
	return sOLT, nil
}

func (a *adapter) setupRequestHandler(ctx context.Context, coreInstanceID string, iadapter adapters.IAdapter) error {
	log.Info("setting-request-handler")
	requestProxy := com.NewRequestHandlerProxy(coreInstanceID, iadapter, a.coreProxy)
	if err := a.kip.SubscribeWithRequestHandlerInterface(kafka.Topic{Name: a.config.Topic}, requestProxy); err != nil {
		log.Errorw("request-handler-setup-failed", log.Fields{"error": err})
		return err

	}
	probe.UpdateStatusFromContext(ctx, "core-request-handler", probe.ServiceStatusRunning)
	log.Info("request-handler-setup-done")
	return nil
}

func (a *adapter) registerWithCore(retries int) error {
	log.Info("registering-with-core")
	adapterDescription := &voltha.Adapter{Id: "openolt", // Unique name for the device type
		Vendor:  "VOLTHA OpenOLT",
		Version: version.VersionInfo.Version}
	types := []*voltha.DeviceType{{Id: "openolt",
		Adapter:                     "openolt", // Name of the adapter that handles device type
		AcceptsBulkFlowUpdate:       false,     // Currently openolt adapter does not support bulk flow handling
		AcceptsAddRemoveFlowUpdates: true}}
	deviceTypes := &voltha.DeviceTypes{Items: types}
	count := 0
	for {
		if err := a.coreProxy.RegisterAdapter(context.TODO(), adapterDescription, deviceTypes); err != nil {
			log.Warnw("registering-with-core-failed", log.Fields{"error": err})
			if retries == count {
				return err
			}
			count++
			// Take a nap before retrying
			time.Sleep(2 * time.Second)
		} else {
			break
		}
	}
	log.Info("registered-with-core")
	return nil
}

func waitForExit() int {
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	exitChannel := make(chan int)

	go func() {
		s := <-signalChannel
		switch s {
		case syscall.SIGHUP,
			syscall.SIGINT,
			syscall.SIGTERM,
			syscall.SIGQUIT:
			log.Infow("closing-signal-received", log.Fields{"signal": s})
			exitChannel <- 0
		default:
			log.Infow("unexpected-signal-received", log.Fields{"signal": s})
			exitChannel <- 1
		}
	}()

	code := <-exitChannel
	return code
}

func printBanner() {
	fmt.Println("   ____                     ____  _   _______ ")
	fmt.Println("  / _ \\                   / __\\| | |__   __|")
	fmt.Println(" | |  | |_ __   ___ _ __  | |  | | |    | |   ")
	fmt.Println(" | |  | | '_\\ / _\\ '_\\ | |  | | |    | |   ")
	fmt.Println(" | |__| | |_) |  __/ | | || |__| | |____| |   ")
	fmt.Println(" \\____/| .__/\\___|_| |_|\\____/|______|_|   ")
	fmt.Println("        | |                                   ")
	fmt.Println("        |_|                                   ")
	fmt.Println("                                              ")
}

func printVersion() {
	fmt.Println("VOLTHA OpenOLT Adapter")
	fmt.Println(version.VersionInfo.String("  "))
}

func main() {
	start := time.Now()

	cf := config.NewAdapterFlags()
	cf.ParseCommandArguments()

	// Setup logging

	// Setup default logger - applies for packages that do not have specific logger set
	if _, err := log.SetDefaultLogger(log.JSON, cf.LogLevel, log.Fields{"instanceID": cf.InstanceID}); err != nil {
		log.With(log.Fields{"error": err}).Fatal("Cannot setup logging")
	}

	// Update all loggers (provisionned via init) with a common field
	if err := log.UpdateAllLoggers(log.Fields{"instanceID": cf.InstanceID}); err != nil {
		log.With(log.Fields{"error": err}).Fatal("Cannot setup logging")
	}

	log.SetPackageLogLevel("github.com/opencord/voltha-go/adapters/common", log.DebugLevel)

	defer log.CleanUp()

	// Print version / build information and exit
	if cf.DisplayVersionOnly {
		printVersion()
		return
	}

	// Print banner if specified
	if cf.Banner {
		printBanner()
	}

	log.Infow("config", log.Fields{"config": *cf})

	// create new adapter
	ad := newAdapter(cf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := &probe.Probe{}
	go p.ListenAndServe(fmt.Sprintf("%s:%d", ad.config.ProbeHost, ad.config.ProbePort))

	probeCtx := context.WithValue(ctx, probe.ProbeContextKey, p)

	go ad.start(probeCtx)

	code := waitForExit()
	log.Infow("received-a-closing-signal", log.Fields{"code": code})

	// Cleanup before leaving
	ad.stop(probeCtx)

	elapsed := time.Since(start)
	log.Infow("run-time", log.Fields{"instanceID": ad.config.InstanceID, "time": elapsed / time.Second})
}
