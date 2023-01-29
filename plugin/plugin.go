package plugin

import (
	"net"

	gerr "github.com/gatewayd-io/gatewayd/errors"
	pluginV1 "github.com/gatewayd-io/gatewayd/plugin/v1"
	goplugin "github.com/hashicorp/go-plugin"
)

type Plugin interface {
	Start() (net.Addr, error)
	Stop()
	Dispense() (pluginV1.GatewayDPluginServiceClient, error)
}

type Identifier struct {
	Name      string
	Version   string
	RemoteURL string
	Checksum  string
}

type Impl struct {
	goplugin.NetRPCUnsupportedPlugin
	pluginV1.GatewayDPluginServiceServer

	client *goplugin.Client

	ID          Identifier
	Description string
	Authors     []string
	License     string
	ProjectURL  string
	LocalPath   string
	Enabled     bool
	// internal and external config options
	Config map[string]string
	// hooks it attaches to
	Hooks    []HookType
	Priority Priority
	// required plugins to be loaded before this one
	// Built-in plugins are always loaded first
	Requires   []Identifier
	Tags       []string
	Categories []string
}

var _ Plugin = &Impl{}

func (p *Impl) Start() (net.Addr, error) {
	var addr net.Addr
	var err error
	if addr, err = p.client.Start(); err != nil {
		return nil, err //nolint:wrapcheck
	}
	return addr, nil
}

func (p *Impl) Stop() {
	p.client.Kill()
}

func (p *Impl) Dispense() (pluginV1.GatewayDPluginServiceClient, error) {
	rpcClient, err := p.client.Client()
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	raw, err := rpcClient.Dispense(p.ID.Name)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if gatewaydPlugin, ok := raw.(pluginV1.GatewayDPluginServiceClient); ok {
		return gatewaydPlugin, nil
	}

	return nil, gerr.ErrPluginNotReady
}