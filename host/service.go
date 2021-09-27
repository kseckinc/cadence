// Copyright (c) 2021 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package host

import (
	"math/rand"
	"os"
	"sync/atomic"
	"time"

	"github.com/uber-go/tally"
	"go.uber.org/yarpc"

	"github.com/uber/cadence/client"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/archiver"
	"github.com/uber/cadence/common/archiver/provider"
	"github.com/uber/cadence/common/blobstore"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/membership"
	"github.com/uber/cadence/common/messaging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/rpc"
	"github.com/uber/cadence/common/service"
)

type (
	// Service is the interface which must be implemented by all the services
	// TODO: Service contains many methods that are not used now that we have resource bean, these should be cleaned up
	Service interface {
		GetHostName() string
		Start()
		Stop()
		GetLogger() log.Logger
		GetThrottledLogger() log.Logger
		GetMetricsClient() metrics.Client
		GetClientBean() client.Bean
		GetTimeSource() clock.TimeSource
		GetDispatcher() *yarpc.Dispatcher
		GetMembershipMonitor() membership.Monitor
		GetHostInfo() *membership.HostInfo
		GetClusterMetadata() cluster.Metadata
		GetMessagingClient() messaging.Client
		GetBlobstoreClient() blobstore.Client
		GetArchivalMetadata() archiver.ArchivalMetadata
		GetArchiverProvider() provider.ArchiverProvider
		GetPayloadSerializer() persistence.PayloadSerializer
	}

	// Service contains the objects specific to this service
	serviceImpl struct {
		status                int32
		sName                 string
		hostName              string
		hostInfo              *membership.HostInfo
		dispatcher            *yarpc.Dispatcher
		membershipFactory     service.MembershipMonitorFactory
		membershipMonitor     membership.Monitor
		rpcFactory            common.RPCFactory
		pprofInitializer      common.PProfInitializer
		clientBean            client.Bean
		timeSource            clock.TimeSource
		numberOfHistoryShards int

		logger          log.Logger
		throttledLogger log.Logger

		metricsScope           tally.Scope
		runtimeMetricsReporter *metrics.RuntimeMetricsReporter
		metricsClient          metrics.Client
		clusterMetadata        cluster.Metadata
		messagingClient        messaging.Client
		blobstoreClient        blobstore.Client
		dynamicCollection      *dynamicconfig.Collection
		dispatcherProvider     rpc.DispatcherProvider
		archivalMetadata       archiver.ArchivalMetadata
		archiverProvider       provider.ArchiverProvider
		serializer             persistence.PayloadSerializer
	}
)

var _ Service = (*serviceImpl)(nil)

// NewService instantiates a Service Instance
// TODO: have a better name for Service.
func NewService(params *service.BootstrapParams) Service {
	sVice := &serviceImpl{
		status:                common.DaemonStatusInitialized,
		sName:                 params.Name,
		logger:                params.Logger,
		throttledLogger:       params.ThrottledLogger,
		rpcFactory:            params.RPCFactory,
		membershipFactory:     params.MembershipFactory,
		pprofInitializer:      params.PProfInitializer,
		timeSource:            clock.NewRealTimeSource(),
		metricsScope:          params.MetricScope,
		numberOfHistoryShards: params.PersistenceConfig.NumHistoryShards,
		clusterMetadata:       params.ClusterMetadata,
		metricsClient:         params.MetricsClient,
		messagingClient:       params.MessagingClient,
		blobstoreClient:       params.BlobstoreClient,
		dispatcherProvider:    params.DispatcherProvider,
		dynamicCollection: dynamicconfig.NewCollection(
			params.DynamicConfig,
			params.Logger,
			dynamicconfig.ClusterNameFilter(params.ClusterMetadata.GetCurrentClusterName()),
		),
		archivalMetadata: params.ArchivalMetadata,
		archiverProvider: params.ArchiverProvider,
		serializer:       persistence.NewPayloadSerializer(),
	}

	sVice.runtimeMetricsReporter = metrics.NewRuntimeMetricsReporter(params.MetricScope, time.Minute, sVice.GetLogger(), params.InstanceID)
	sVice.dispatcher = sVice.rpcFactory.GetDispatcher()
	if sVice.dispatcher == nil {
		sVice.logger.Fatal("Unable to create yarpc dispatcher")
	}

	// Get the host name and set it on the service.  This is used for emitting metric with a tag for hostname
	if hostName, err := os.Hostname(); err != nil {
		sVice.logger.WithTags(tag.Error(err)).Fatal("Error getting hostname")
	} else {
		sVice.hostName = hostName
	}
	return sVice
}

// GetHostName returns the name of host running the service
func (h *serviceImpl) GetHostName() string {
	return h.hostName
}

// Start starts a yarpc service
func (h *serviceImpl) Start() {
	if !atomic.CompareAndSwapInt32(&h.status, common.DaemonStatusInitialized, common.DaemonStatusStarted) {
		return
	}

	var err error

	h.metricsScope.Counter(metrics.RestartCount).Inc(1)
	h.runtimeMetricsReporter.Start()

	if err := h.pprofInitializer.Start(); err != nil {
		h.logger.WithTags(tag.Error(err)).Fatal("Failed to start pprof")
	}

	if err := h.dispatcher.Start(); err != nil {
		h.logger.WithTags(tag.Error(err)).Fatal("Failed to start yarpc dispatcher")
	}

	h.membershipMonitor, err = h.membershipFactory.GetMembershipMonitor()
	if err != nil {
		h.logger.WithTags(tag.Error(err)).Fatal("Membership monitor creation failed")
	}

	h.membershipMonitor.Start()

	hostInfo, err := h.membershipMonitor.WhoAmI()
	if err != nil {
		h.logger.WithTags(tag.Error(err)).Fatal("failed to get host info from membership monitor")
	}
	h.hostInfo = hostInfo

	h.clientBean, err = client.NewClientBean(
		client.NewRPCClientFactory(h.rpcFactory, h.membershipMonitor, h.metricsClient, h.dynamicCollection, h.numberOfHistoryShards, h.logger),
		h.dispatcherProvider,
		h.clusterMetadata,
	)
	if err != nil {
		h.logger.WithTags(tag.Error(err)).Fatal("fail to initialize client bean")
	}

	// The service is now started up
	h.logger.Info("service started")
	// seed the random generator once for this service
	rand.Seed(time.Now().UTC().UnixNano())
}

// Stop closes the associated transport
func (h *serviceImpl) Stop() {
	if !atomic.CompareAndSwapInt32(&h.status, common.DaemonStatusStarted, common.DaemonStatusStopped) {
		return
	}

	if h.membershipMonitor != nil {
		h.membershipMonitor.Stop()
	}

	if h.dispatcher != nil {
		h.dispatcher.Stop() //nolint:errcheck
	}

	h.runtimeMetricsReporter.Stop()
}

func (h *serviceImpl) GetLogger() log.Logger {
	return h.logger
}

func (h *serviceImpl) GetThrottledLogger() log.Logger {
	return h.throttledLogger
}

func (h *serviceImpl) GetMetricsClient() metrics.Client {
	return h.metricsClient
}

func (h *serviceImpl) GetClientBean() client.Bean {
	return h.clientBean
}

func (h *serviceImpl) GetTimeSource() clock.TimeSource {
	return h.timeSource
}

func (h *serviceImpl) GetMembershipMonitor() membership.Monitor {
	return h.membershipMonitor
}

func (h *serviceImpl) GetHostInfo() *membership.HostInfo {
	return h.hostInfo
}

func (h *serviceImpl) GetDispatcher() *yarpc.Dispatcher {
	return h.dispatcher
}

// GetClusterMetadata returns the service cluster metadata
func (h *serviceImpl) GetClusterMetadata() cluster.Metadata {
	return h.clusterMetadata
}

// GetMessagingClient returns the messaging client against Kafka
func (h *serviceImpl) GetMessagingClient() messaging.Client {
	return h.messagingClient
}

func (h *serviceImpl) GetBlobstoreClient() blobstore.Client {
	return h.blobstoreClient
}

func (h *serviceImpl) GetArchivalMetadata() archiver.ArchivalMetadata {
	return h.archivalMetadata
}

func (h *serviceImpl) GetArchiverProvider() provider.ArchiverProvider {
	return h.archiverProvider
}

func (h *serviceImpl) GetPayloadSerializer() persistence.PayloadSerializer {
	return h.serializer
}
