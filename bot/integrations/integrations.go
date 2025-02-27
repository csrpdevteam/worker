package integrations

import (
	"github.com/csrpdevteam/common/integrations/bloxlink"
	"github.com/csrpdevteam/common/webproxy"
	"github.com/csrpdevteam/worker/bot/redis"
	"github.com/csrpdevteam/worker/config"
)

var (
	WebProxy    *webproxy.WebProxy
	SecureProxy *SecureProxyClient
	Bloxlink    *bloxlink.BloxlinkIntegration
)

func InitIntegrations() {
	WebProxy = webproxy.NewWebProxy(config.Conf.WebProxy.Url, config.Conf.WebProxy.AuthHeaderName, config.Conf.WebProxy.AuthHeaderValue)
	Bloxlink = bloxlink.NewBloxlinkIntegration(redis.Client, WebProxy, config.Conf.Integrations.BloxlinkApiKey)
	SecureProxy = NewSecureProxy(config.Conf.Integrations.SecureProxyUrl)
}
