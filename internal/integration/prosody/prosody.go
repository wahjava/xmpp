// Copyright 2020 The Mellium Contributors.
// Use of this source code is governed by the BSD 2-clause
// license that can be found in the LICENSE file.

// Package prosody facilitates integration testing against Prosody.
package prosody // import "mellium.im/xmpp/internal/integration/prosody"

import (
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"testing"

	"mellium.im/xmpp/internal/integration"
	"mellium.im/xmpp/jid"
)

const (
	cfgFileName = "prosody.cfg.lua"
	cmdName     = "prosody"
	configFlag  = "--config"
)

// New creates a new, unstarted, prosody daemon.
//
// The provided context is used to kill the process (by calling os.Process.Kill)
// if the context becomes done before the command completes on its own.
func New(ctx context.Context, opts ...integration.Option) (*integration.Cmd, error) {
	return integration.New(
		ctx, cmdName,
		opts...,
	)
}

// ConfigFile is an option that can be used to write a temporary Prosody config
// file.
// This will overwrite the existing config file and make most of the other
// options in this package noops.
// This option only exists for the rare occasion that you need complete control
// over the config file.
func ConfigFile(cfg Config) integration.Option {
	return func(cmd *integration.Cmd) error {
		cmd.Config = cfg
		err := integration.TempFile(cfgFileName, func(cmd *integration.Cmd, w io.Writer) error {
			return cfgTmpl.Execute(w, struct {
				Config
				ConfigDir string
			}{
				Config:    cfg,
				ConfigDir: cmd.ConfigDir(),
			})
		})(cmd)
		if err != nil {
			return err
		}
		cfgFilePath := filepath.Join(cmd.ConfigDir(), cfgFileName)
		return integration.Args(configFlag, cfgFilePath)(cmd)
	}
}

// Ctl returns an option that calls prosodyctl with the provided args.
// It automatically points prosodyctl at the config file so there is no need to
// pass the --config option.
func Ctl(ctx context.Context, args ...string) integration.Option {
	return integration.Defer(func(cmd *integration.Cmd) error {
		cfgFilePath := filepath.Join(cmd.ConfigDir(), cfgFileName)
		/* #nosec */
		prosodyCtl := exec.CommandContext(ctx, "prosodyctl", configFlag, cfgFilePath)
		prosodyCtl.Args = append(prosodyCtl.Args, args...)
		err := prosodyCtl.Run()
		return err
	})
}

func getConfig(cmd *integration.Cmd) Config {
	if cmd.Config == nil {
		cmd.Config = Config{}
	}
	return cmd.Config.(Config)
}

// ListenC2S listens for client-to-server (c2s) connections on a random port.
func ListenC2S() integration.Option {
	return func(cmd *integration.Cmd) error {
		c2sListener, err := cmd.C2SListen("tcp", "[::1]:0")
		if err != nil {
			return err
		}
		// Prosody creates its own sockets and doesn't provide us with a way of
		// pointing it at an existing Unix domain socket or handing the filehandle for
		// the TCP connection to it on start, so we're effectively just listening to
		// get a random port that we'll use to configure Prosody, then we need to
		// close the connection and let Prosody listen on that port.
		// Technically this is racey, but it's not likely to be a problem in practice.
		c2sPort := c2sListener.Addr().(*net.TCPAddr).Port
		err = c2sListener.Close()
		if err != nil {
			return err
		}

		cfg := getConfig(cmd)
		cfg.C2SPort = c2sPort
		cmd.Config = cfg
		return nil
	}
}

// ListenS2S listens for server-to-server (s2s) connections on a random port.
func ListenS2S() integration.Option {
	return func(cmd *integration.Cmd) error {
		s2sListener, err := cmd.S2SListen("tcp", "[::1]:0")
		if err != nil {
			return err
		}
		// Prosody creates its own sockets and doesn't provide us with a way of
		// pointing it at an existing Unix domain socket or handing the filehandle for
		// the TCP connection to it on start, so we're effectively just listening to
		// get a random port that we'll use to configure Prosody, then we need to
		// close the connection and let Prosody listen on that port.
		// Technically this is racey, but it's not likely to be a problem in practice.
		s2sPort := s2sListener.Addr().(*net.TCPAddr).Port
		err = s2sListener.Close()
		if err != nil {
			return err
		}

		cfg := getConfig(cmd)
		cfg.S2SPort = s2sPort
		cmd.Config = cfg
		return nil
	}
}

// VHost configures one or more virtual hosts.
// The default if this option is not provided is to create a single vhost called
// "localhost" and create a self-signed cert for it (if VHost is specified certs
// must be manually created).
func VHost(hosts ...string) integration.Option {
	return func(cmd *integration.Cmd) error {
		cfg := getConfig(cmd)
		cfg.VHosts = append(cfg.VHosts, hosts...)
		cmd.Config = cfg
		return nil
	}
}

// CreateUser returns an option that calls prosodyctl to create a user.
// It is equivalent to calling:
// Ctl(ctx, "register", "localpart", "domainpart", "password") except that it
// also configures the underlying Cmd to know about the user.
func CreateUser(ctx context.Context, addr, pass string) integration.Option {
	return func(cmd *integration.Cmd) error {
		j, err := jid.Parse(addr)
		if err != nil {
			return err
		}
		err = Ctl(ctx, "register", j.Localpart(), j.Domainpart(), pass)(cmd)
		if err != nil {
			return err
		}
		return integration.User(j, pass)(cmd)
	}
}

// Modules adds custom modules to the enabled modules list.
func Modules(mod ...string) integration.Option {
	return func(cmd *integration.Cmd) error {
		cfg := getConfig(cmd)
		for _, m := range mod {
			cfg.Modules = append(cfg.Modules, m)
		}
		cmd.Config = cfg
		return nil
	}
}

// TrustAll configures prosody to trust all certificates presented to it without
// any verification.
func TrustAll() integration.Option {
	const modName = "trustall"
	return func(cmd *integration.Cmd) error {
		err := Modules(modName)(cmd)
		if err != nil {
			return err
		}
		return integration.TempFile("mod_"+modName+".lua", func(_ *integration.Cmd, w io.Writer) error {
			_, err := io.WriteString(w, `
module:set_global();

module:hook("s2s-check-certificate", function(event)
	local session = event.session;
	module:log("info", "implicitly trusting presented certificate");
	session.cert_chain_status = "valid";
	session.cert_identity_status = "valid";
	return true;
end);`)
			return err
		})(cmd)
	}
}

func defaultConfig(cmd *integration.Cmd) error {
	for _, arg := range cmd.Cmd.Args {
		if arg == configFlag {
			return nil
		}
	}

	cfg := getConfig(cmd)
	if len(cfg.VHosts) == 0 {
		const vhost = "localhost"
		cfg.VHosts = append(cfg.VHosts, vhost)
		err := integration.Cert(vhost)(cmd)
		if err != nil {
			return err
		}
	}
	cmd.Config = cfg
	if j, _ := cmd.User(); j.Equal(jid.JID{}) {
		err := CreateUser(context.TODO(), "me@"+cfg.VHosts[0], "password")(cmd)
		if err != nil {
			return err
		}
	}

	return ConfigFile(cfg)(cmd)
}

// Test starts a Prosody instance and returns a function that runs subtests
// using t.Run.
// Multiple calls to the returned function will result in uniquely named
// subtests.
// When all subtests have completed, the daemon is stopped.
func Test(ctx context.Context, t *testing.T, opts ...integration.Option) integration.SubtestRunner {
	opts = append(opts, defaultConfig)
	return integration.Test(ctx, cmdName, t, opts...)
}
