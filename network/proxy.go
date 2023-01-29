package network

import (
	"context"
	"errors"
	"io"

	gerr "github.com/gatewayd-io/gatewayd/errors"
	"github.com/gatewayd-io/gatewayd/plugin"
	"github.com/gatewayd-io/gatewayd/pool"
	"github.com/panjf2000/gnet/v2"
	"github.com/rs/zerolog"
)

const (
	EmptyPoolCapacity int = 0
)

type Proxy interface {
	Connect(gconn gnet.Conn) *gerr.GatewayDError
	Disconnect(gconn gnet.Conn) *gerr.GatewayDError
	PassThrough(gconn gnet.Conn) *gerr.GatewayDError
	IsHealty(cl *Client) (*Client, *gerr.GatewayDError)
	IsExhausted() bool
	Shutdown()
}

type ProxyImpl struct {
	availableConnections pool.Pool
	busyConnections      pool.Pool
	logger               zerolog.Logger
	hookConfig           *plugin.HookConfig

	Elastic             bool
	ReuseElasticClients bool

	// ClientConfig is used for elastic proxy and reconnection
	ClientConfig *Client
}

var _ Proxy = &ProxyImpl{}

// NewProxy creates a new proxy.
func NewProxy(
	p pool.Pool, hookConfig *plugin.HookConfig,
	elastic, reuseElasticClients bool,
	clientConfig *Client, logger zerolog.Logger,
) *ProxyImpl {
	return &ProxyImpl{
		availableConnections: p,
		busyConnections:      pool.NewPool(EmptyPoolCapacity),
		logger:               logger,
		hookConfig:           hookConfig,
		Elastic:              elastic,
		ReuseElasticClients:  reuseElasticClients,
		ClientConfig:         clientConfig,
	}
}

// Connect maps a server connection from the available connection pool to a incoming connection.
// It returns an error if the pool is exhausted. If the pool is elastic, it creates a new client
// and maps it to the incoming connection.
//
//nolint:funlen
func (pr *ProxyImpl) Connect(gconn gnet.Conn) *gerr.GatewayDError {
	var clientID string
	// Get the first available client from the pool.
	pr.availableConnections.ForEach(func(key, _ interface{}) bool {
		if cid, ok := key.(string); ok {
			clientID = cid
			return false // stop the loop.
		}
		return true
	})

	var client *Client
	if pr.IsExhausted() {
		// Pool is exhausted or is elastic.
		if pr.Elastic {
			// Create a new client.
			client = NewClient(
				pr.ClientConfig.Network,
				pr.ClientConfig.Address,
				pr.ClientConfig.ReceiveBufferSize,
				pr.ClientConfig.ReceiveChunkSize,
				pr.ClientConfig.ReceiveDeadline,
				pr.ClientConfig.SendDeadline,
				pr.logger,
			)
			pr.logger.Debug().Str("id", client.ID[:7]).Msg("Reused the client connection")
		} else {
			return gerr.ErrPoolExhausted
		}
	} else {
		// Get the client from the pool with the given clientID.
		if cl, ok := pr.availableConnections.Pop(clientID).(*Client); ok {
			client = cl
		}
	}

	//
	client, err := pr.IsHealty(client)
	if err != nil {
		pr.logger.Error().Err(err).Msg("Failed to connect to the client")
	}

	if err := pr.busyConnections.Put(gconn, client); err != nil {
		// This should never happen.
		return err
	}
	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.connect",
			"client":   client.ID[:7],
			"server":   gconn.RemoteAddr().String(),
		},
	).Msg("Client has been assigned")

	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.connect",
			"count":    pr.availableConnections.Size(),
		},
	).Msg("Available client connections")
	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.connect",
			"count":    pr.busyConnections.Size(),
		},
	).Msg("Busy client connections")

	return nil
}

// Disconnect removes the client from the busy connection pool and tries to recycle
// the server connection.
func (pr *ProxyImpl) Disconnect(gconn gnet.Conn) *gerr.GatewayDError {
	client := pr.busyConnections.Pop(gconn)
	//nolint:nestif
	if client != nil {
		if client, ok := client.(*Client); ok {
			if (pr.Elastic && pr.ReuseElasticClients) || !pr.Elastic {
				_, err := pr.IsHealty(client)
				if err != nil {
					pr.logger.Error().Err(err).Msg("Failed to reconnect to the client")
				}
				// If the client is not in the pool, put it back.
				err = pr.availableConnections.Put(client.ID, client)
				if err != nil {
					pr.logger.Error().Err(err).Msg("Failed to put the client back in the pool")
				}
			} else {
				return gerr.ErrClientNotConnected
			}
		} else {
			// This should never happen, but if it does,
			// then there are some serious issues with the pool.
			return gerr.ErrCastFailed
		}
	} else {
		return gerr.ErrClientNotFound
	}

	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.disconnect",
			"count":    pr.availableConnections.Size(),
		},
	).Msg("Available client connections")
	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.disconnect",
			"count":    pr.busyConnections.Size(),
		},
	).Msg("Busy client connections")

	return nil
}

// PassThrough sends the data from the client to the server and vice versa.
//
//nolint:funlen
func (pr *ProxyImpl) PassThrough(gconn gnet.Conn) *gerr.GatewayDError {
	// TODO: Handle bi-directional traffic
	// Currently the passthrough is a one-way street from the client to the server, that is,
	// the client can send data to the server and receive the response back, but the server
	// cannot take initiative and send data to the client. So, there should be another event-loop
	// that listens for data from the server and sends it to the client

	var client *Client
	if pr.busyConnections.Get(gconn) == nil {
		return gerr.ErrClientNotFound
	}

	// Get the client from the busy connection pool.
	if cl, ok := pr.busyConnections.Get(gconn).(*Client); ok {
		client = cl
	} else {
		return gerr.ErrCastFailed
	}

	// request contains the data from the client.
	request, origErr := gconn.Next(-1)
	if origErr != nil {
		pr.logger.Error().Err(origErr).Msg("Error reading from client")
	}
	pr.logger.Debug().Fields(
		map[string]interface{}{
			"length": len(request),
			"local":  gconn.LocalAddr().String(),
			"remote": gconn.RemoteAddr().String(),
		},
	).Msg("Received data from client")

	// Run the OnIngressTraffic hooks.
	result, err := pr.hookConfig.Run(
		context.Background(),
		trafficData(gconn, client, "request", request, origErr),
		plugin.OnIngressTraffic,
		pr.hookConfig.Verification)
	if err != nil {
		pr.logger.Error().Err(err).Msg("Error running hook")
	}
	// If the hook modified the request, use the modified request.
	modRequest, errMsg := extractField(result, "request")
	if errMsg != "" {
		pr.logger.Error().Str("error", errMsg).Msg("Error in hook")
	}
	if modRequest != nil {
		request = modRequest
	}

	// Send the request to the server.
	sent, err := client.Send(request)
	if err != nil {
		pr.logger.Error().Err(err).Msg("Error sending request to database")
	}
	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.passthrough",
			"length":   sent,
			"local":    client.Conn.LocalAddr().String(),
			"remote":   client.Conn.RemoteAddr().String(),
		},
	).Msg("Sent data to database")

	// Receive the response from the server.
	received, response, err := client.Receive()
	pr.logger.Debug().Fields(
		map[string]interface{}{
			"function": "proxy.passthrough",
			"length":   received,
			"local":    client.Conn.LocalAddr().String(),
			"remote":   client.Conn.RemoteAddr().String(),
		},
	).Msg("Received data from database")

	// The connection to the server is closed, so we MUST reconnect,
	// otherwise the client will be stuck.
	if received == 0 && err != nil && errors.Is(err.Unwrap(), io.EOF) {
		pr.logger.Debug().Fields(
			map[string]interface{}{
				"function": "proxy.passthrough",
				"local":    client.Conn.LocalAddr().String(),
				"remote":   client.Conn.RemoteAddr().String(),
			}).Msg("Client disconnected")

		client.Close()
		client = NewClient(
			pr.ClientConfig.Network,
			pr.ClientConfig.Address,
			pr.ClientConfig.ReceiveBufferSize,
			pr.ClientConfig.ReceiveChunkSize,
			pr.ClientConfig.ReceiveDeadline,
			pr.ClientConfig.SendDeadline,
			pr.logger,
		)
		pr.busyConnections.Remove(gconn)
		if err := pr.busyConnections.Put(gconn, client); err != nil {
			// This should never happen
			return err
		}
	}

	// Run the OnEgressTraffic hooks.
	result, err = pr.hookConfig.Run(
		context.Background(),
		trafficData(gconn, client, "response", response[:received], err),
		plugin.OnEgressTraffic,
		pr.hookConfig.Verification)
	if err != nil {
		pr.logger.Error().Err(err).Msg("Error running hook")
	}
	// If the hook returns a response, use it instead of the original response.
	modResponse, errMsg := extractField(result, "response")
	if errMsg != "" {
		pr.logger.Error().Str("error", errMsg).Msg("Error in hook")
	}
	if modResponse != nil {
		response = modResponse
		received = len(modResponse)
	}

	// Send the response to the client async.
	origErr = gconn.AsyncWrite(response[:received], func(gconn gnet.Conn, err error) error {
		pr.logger.Debug().Fields(
			map[string]interface{}{
				"function": "proxy.passthrough",
				"length":   received,
				"local":    gconn.LocalAddr().String(),
				"remote":   gconn.RemoteAddr().String(),
			},
		).Msg("Sent data to client")
		return err
	})
	if origErr != nil {
		pr.logger.Error().Err(err).Msg("Error writing to client")
		return gerr.ErrServerSendFailed.Wrap(err)
	}

	return nil
}

// IsHealty checks if the pool is exhausted or the client is disconnected.
func (pr *ProxyImpl) IsHealty(client *Client) (*Client, *gerr.GatewayDError) {
	if pr.IsExhausted() {
		pr.logger.Error().Msg("No more available connections")
		return client, gerr.ErrPoolExhausted
	}

	if !client.IsConnected() {
		pr.logger.Error().Msg("Client is disconnected")
	}

	return client, nil
}

// IsExhausted checks if the available connection pool is exhausted.
func (pr *ProxyImpl) IsExhausted() bool {
	if pr.Elastic {
		return false
	}

	return pr.availableConnections.Size() == 0 && pr.availableConnections.Cap() > 0
}

// Shutdown closes all connections and clears the connection pools.
func (pr *ProxyImpl) Shutdown() {
	pr.availableConnections.ForEach(func(key, value interface{}) bool {
		if cl, ok := value.(*Client); ok {
			cl.Close()
		}
		return true
	})
	pr.availableConnections.Clear()
	pr.logger.Debug().Msg("All available connections have been closed")

	pr.busyConnections.ForEach(func(key, value interface{}) bool {
		if gconn, ok := key.(gnet.Conn); ok {
			gconn.Close()
		}
		if cl, ok := value.(*Client); ok {
			cl.Close()
		}
		return true
	})
	pr.busyConnections.Clear()
	pr.logger.Debug().Msg("All busy connections have been closed")
}
