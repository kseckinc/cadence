// Copyright (c) 2017 Uber Technologies, Inc.
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

package cli

import (
	"context"
	"time"

	"github.com/olivere/elastic"
	"github.com/urfave/cli"
	"go.uber.org/yarpc"
	"go.uber.org/yarpc/api/transport"
	"go.uber.org/yarpc/transport/tchannel"
	"go.uber.org/zap"

	clientFrontend "go.uber.org/cadence/.gen/go/cadence/workflowserviceclient"

	serverAdmin "github.com/uber/cadence/.gen/go/admin/adminserviceclient"
	serverFrontend "github.com/uber/cadence/.gen/go/cadence/workflowserviceclient"
	"github.com/uber/cadence/client/admin"
	"github.com/uber/cadence/client/frontend"
	"github.com/uber/cadence/common"
	cc "github.com/uber/cadence/common/client"
)

const (
	cadenceClientName      = "cadence-client"
	cadenceFrontendService = "cadence-frontend"
)

// ContextKey is an alias for string, used as context key
type ContextKey string

const (
	// CtxKeyJWT is the name of the context key for the JWT
	CtxKeyJWT = ContextKey("ctxKeyJWT")
)

// ClientFactory is used to construct rpc clients
type ClientFactory interface {
	ClientFrontendClient(c *cli.Context) clientFrontend.Interface
	ServerFrontendClient(c *cli.Context) frontend.Client
	ServerAdminClient(c *cli.Context) admin.Client

	ElasticSearchClient(c *cli.Context) *elastic.Client
}

type clientFactory struct {
	hostPort   string
	dispatcher *yarpc.Dispatcher
	logger     *zap.Logger
}

// NewClientFactory creates a new ClientFactory
func NewClientFactory() ClientFactory {
	logger, err := zap.NewDevelopment()
	if err != nil {
		panic(err)
	}

	return &clientFactory{
		logger: logger,
	}
}

// ClientFrontendClient builds a frontend client
func (b *clientFactory) ClientFrontendClient(c *cli.Context) clientFrontend.Interface {
	b.ensureDispatcher(c)
	return clientFrontend.New(b.dispatcher.ClientConfig(cadenceFrontendService))
}

// ServerFrontendClient builds a frontend client (based on server side thrift interface)
func (b *clientFactory) ServerFrontendClient(c *cli.Context) frontend.Client {
	b.ensureDispatcher(c)
	return frontend.NewThriftClient(serverFrontend.New(b.dispatcher.ClientConfig(cadenceFrontendService)))
}

// ServerAdminClient builds an admin client (based on server side thrift interface)
func (b *clientFactory) ServerAdminClient(c *cli.Context) admin.Client {
	b.ensureDispatcher(c)
	return admin.NewThriftClient(serverAdmin.New(b.dispatcher.ClientConfig(cadenceFrontendService)))
}

// ElasticSearchClient builds an ElasticSearch client
func (b *clientFactory) ElasticSearchClient(c *cli.Context) *elastic.Client {
	url := getRequiredOption(c, FlagURL)
	retrier := elastic.NewBackoffRetrier(elastic.NewExponentialBackoff(128*time.Millisecond, 513*time.Millisecond))

	client, err := elastic.NewClient(
		elastic.SetURL(url),
		elastic.SetRetrier(retrier),
	)
	if err != nil {
		b.logger.Fatal("Unable to create ElasticSearch client", zap.Error(err))
	}

	return client
}

func (b *clientFactory) ensureDispatcher(c *cli.Context) {
	if b.dispatcher != nil {
		return
	}

	b.hostPort = localHostPort
	if addr := c.GlobalString(FlagAddress); addr != "" {
		b.hostPort = addr
	}

	ch, err := tchannel.NewChannelTransport(tchannel.ServiceName(cadenceClientName), tchannel.ListenAddr("127.0.0.1:0"))
	if err != nil {
		b.logger.Fatal("Failed to create transport channel", zap.Error(err))
	}

	b.dispatcher = yarpc.NewDispatcher(yarpc.Config{
		Name: cadenceClientName,
		Outbounds: yarpc.Outbounds{
			cadenceFrontendService: {Unary: ch.NewSingleOutbound(b.hostPort)},
		},
		OutboundMiddleware: yarpc.OutboundMiddleware{
			Unary: &versionMiddleware{},
		},
	})

	if err := b.dispatcher.Start(); err != nil {
		b.dispatcher.Stop()
		b.logger.Fatal("Failed to create outbound transport channel: %v", zap.Error(err))
	}
}

type versionMiddleware struct {
}

func (vm *versionMiddleware) Call(ctx context.Context, request *transport.Request, out transport.UnaryOutbound) (*transport.Response, error) {
	request.Headers = request.Headers.
		With(common.ClientImplHeaderName, cc.CLI).
		With(common.FeatureVersionHeaderName, cc.SupportedCLIVersion)
	if jwtKey, ok := ctx.Value(CtxKeyJWT).(string); ok {
		request.Headers = request.Headers.With(common.AuthorizationTokenHeaderName, jwtKey)
	}
	return out.Call(ctx, request)
}

func getJWT(c *cli.Context) string {
	return c.GlobalString(FlagJWT)
}

func getJWTPrivateKey(c *cli.Context) string {
	return c.GlobalString(FlagJWTPrivateKey)
}
