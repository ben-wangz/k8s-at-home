package main

import (
	"os"

	"github.com/tg123/sshpiper/libplugin"
	"github.com/urfave/cli/v2"
)

func main() {
	libplugin.CreateAndRunPluginTemplate(&libplugin.PluginTemplate{
		Name:  "routeauth",
		Usage: "sshpiperd routeauth plugin",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "auth-mode",
				Usage:   "authentication mode: password or key",
				EnvVars: []string{"SSHPIPER_AUTH_MODE"},
				Value:   "password",
			},
			&cli.StringFlag{
				Name:    "allowed-targets",
				Usage:   "comma separated allowed target patterns",
				EnvVars: []string{"SSHPIPER_ALLOWED_TARGETS"},
			},
			&cli.StringFlag{
				Name:    "hostkey-policy",
				Usage:   "host key policy: tofu, strict or insecure",
				EnvVars: []string{"SSHPIPER_HOSTKEY_POLICY"},
				Value:   "tofu",
			},
			&cli.StringFlag{
				Name:    "known-hosts-path",
				Usage:   "known_hosts file path",
				EnvVars: []string{"SSHPIPER_KNOWN_HOSTS_PATH"},
				Value:   "/var/sshpiper/known_hosts/known_hosts",
			},
			&cli.StringFlag{
				Name:    "authorized-keys-path",
				Usage:   "authorized_keys file path for downstream public key auth",
				EnvVars: []string{"SSHPIPER_AUTHORIZED_KEYS_PATH"},
				Value:   "/auth/authorized_keys",
			},
			&cli.StringFlag{
				Name:    "upstream-private-key-path",
				Usage:   "private key path for upstream public key auth",
				EnvVars: []string{"SSHPIPER_UPSTREAM_PRIVATE_KEY_PATH"},
				Value:   "/auth/id_rsa",
			},
		},
		CreateConfig: func(c *cli.Context) (*libplugin.SshPiperPluginConfig, error) {
			plugin, err := newRouteAuthPlugin(routeAuthConfig{
				authMode:               c.String("auth-mode"),
				allowedTargets:         c.String("allowed-targets"),
				hostKeyPolicy:          c.String("hostkey-policy"),
				knownHostsPath:         c.String("known-hosts-path"),
				authorizedKeysPath:     c.String("authorized-keys-path"),
				upstreamPrivateKeyPath: c.String("upstream-private-key-path"),
			})
			if err != nil {
				return nil, err
			}

			_ = os.Setenv("SSHPIPER_ROUTEAUTH_ACTIVE", "true")
			return plugin.CreateConfig(), nil
		},
	})
}
