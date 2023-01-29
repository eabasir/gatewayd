package network

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"syscall"

	gerr "github.com/gatewayd-io/gatewayd/errors"
	"github.com/panjf2000/gnet/v2"
	"github.com/rs/zerolog"
)

// GetRLimit returns the current system soft and hard limits for the number of open files.
func GetRLimit(logger zerolog.Logger) syscall.Rlimit {
	var limits syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limits); err != nil { //nolint:nosnakecase
		logger.Error().Err(err).Msg("Failed to get rlimit")
	}
	logger.Debug().Str("value", fmt.Sprint(limits.Cur)).Msg("Current system soft limit")
	logger.Debug().Str("value", fmt.Sprint(limits.Max)).Msg("Current system hard limit")
	return limits
}

// GetID returns a unique ID (hash) for a network connection.
func GetID(network, address string, seed int, logger zerolog.Logger) string {
	hash := sha256.New()
	_, err := hash.Write([]byte(fmt.Sprintf("%s://%s%d", network, address, seed)))
	if err != nil {
		logger.Error().Err(err).Msg("Failed to generate ID")
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Resolve resolves a network address.
func Resolve(network, address string, logger zerolog.Logger) (string, *gerr.GatewayDError) {
	switch network {
	case "tcp", "tcp4", "tcp6":
		addr, err := net.ResolveTCPAddr(network, address)
		if err == nil {
			return addr.String(), nil
		}
		return "", gerr.ErrResolveFailed.Wrap(err)
	case "udp", "udp4", "udp6":
		addr, err := net.ResolveUDPAddr(network, address)
		if err == nil {
			return addr.String(), nil
		}
		return "", gerr.ErrResolveFailed.Wrap(err)
	case "unix", "unixgram", "unixpacket":
		addr, err := net.ResolveUnixAddr(network, address)
		if err == nil {
			return addr.String(), nil
		}
		return "", gerr.ErrResolveFailed.Wrap(err)
	default:
		logger.Error().Str("network", network).Msg("Network is not supported")
		return "", gerr.ErrNetworkNotSupported
	}
}

// trafficData creates the ingress/egress map for the OnIngressTraffic/OnEgresTraffic hooks.
func trafficData(
	gconn gnet.Conn,
	client *Client,
	fieldName string,
	fieldValue []byte,
	err interface{},
) map[string]interface{} {
	data := map[string]interface{}{
		"client": map[string]interface{}{
			"local":  gconn.LocalAddr().String(),
			"remote": gconn.RemoteAddr().String(),
		},
		"server": map[string]interface{}{
			"local":  client.Conn.LocalAddr().String(),
			"remote": client.Conn.RemoteAddr().String(),
		},
		fieldName: fieldValue, // Will be converted to base64-encoded string.
		"error":   "",
	}

	if err != nil {
		switch typedErr := err.(type) {
		case *gerr.GatewayDError:
		case error:
			data["error"] = typedErr.Error()
		case string:
			data["error"] = typedErr
		default:
			data["error"] = fmt.Sprintf("%v", err)
		}
	}

	return data
}

// extractField extracts the given field name and error message from the result of the hook.
func extractField(result map[string]interface{}, fieldName string) ([]byte, string) {
	var data []byte
	var err string
	if result != nil {
		if fieldValue, ok := result[fieldName].([]byte); ok {
			data = fieldValue
		}
		if errMsg, ok := result["error"].(string); ok && errMsg != "" {
			err = errMsg
		}
	}
	return data, err
}
