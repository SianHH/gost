// Package register pulls in the built-in components by side effect: every
// component package registers its constructor in init(), and blank imports are
// what trigger them.
//
// This file is the *mobile* set. It is the same list as gost/cmd/gost/register.go
// minus the components that cannot work inside an Android application sandbox,
// where an app runs unprivileged and has no /dev/net/tun, no CAP_NET_ADMIN, no
// CAP_NET_RAW and no usable stdio:
//
//	handler/tun, handler/tungo, handler/tap
//	listener/tun, listener/tungo, listener/tap, listener/stdio
//	dialer/serial, dialer/icmp
//	listener/serial, listener/icmp
//	connector/serial
//	handler/redirect/tcp, handler/redirect/udp
//	listener/redirect/tcp, listener/redirect/udp
//
// Dropping them also drops handler/tungo's transitive gvisor + tun2socks +
// gopacket dependency tree, which is a large share of the library size for
// something that could never run anyway.
//
// Build with -tags gost_all for the full CLI-identical set (register_all.go)
// when you need to check whether a protocol is missing or the set is at fault.
package register

import (
	// Register connectors
	_ "github.com/go-gost/x/connector/direct"
	_ "github.com/go-gost/x/connector/forward"
	_ "github.com/go-gost/x/connector/http"
	_ "github.com/go-gost/x/connector/http2"
	_ "github.com/go-gost/x/connector/masque"
	_ "github.com/go-gost/x/connector/relay"
	_ "github.com/go-gost/x/connector/router"
	_ "github.com/go-gost/x/connector/sni"
	_ "github.com/go-gost/x/connector/socks/v4"
	_ "github.com/go-gost/x/connector/socks/v5"
	_ "github.com/go-gost/x/connector/ss"
	_ "github.com/go-gost/x/connector/ss/udp"
	_ "github.com/go-gost/x/connector/sshd"
	_ "github.com/go-gost/x/connector/tcp"
	_ "github.com/go-gost/x/connector/tunnel"
	_ "github.com/go-gost/x/connector/unix"

	// Register dialers
	_ "github.com/go-gost/x/dialer/direct"
	_ "github.com/go-gost/x/dialer/dtls"
	_ "github.com/go-gost/x/dialer/ftcp"
	_ "github.com/go-gost/x/dialer/grpc"
	_ "github.com/go-gost/x/dialer/http2"
	_ "github.com/go-gost/x/dialer/http2/h2"
	_ "github.com/go-gost/x/dialer/http3"
	_ "github.com/go-gost/x/dialer/http3/masque"
	_ "github.com/go-gost/x/dialer/http3/wt"
	_ "github.com/go-gost/x/dialer/kcp"
	_ "github.com/go-gost/x/dialer/mtcp"
	_ "github.com/go-gost/x/dialer/mtls"
	_ "github.com/go-gost/x/dialer/mws"
	_ "github.com/go-gost/x/dialer/obfs/http"
	_ "github.com/go-gost/x/dialer/obfs/tls"
	_ "github.com/go-gost/x/dialer/pht"
	_ "github.com/go-gost/x/dialer/quic"
	_ "github.com/go-gost/x/dialer/ssh"
	_ "github.com/go-gost/x/dialer/sshd"
	_ "github.com/go-gost/x/dialer/tcp"
	_ "github.com/go-gost/x/dialer/tls"
	_ "github.com/go-gost/x/dialer/udp"
	_ "github.com/go-gost/x/dialer/unix"
	_ "github.com/go-gost/x/dialer/utls"
	_ "github.com/go-gost/x/dialer/ws"
	_ "github.com/go-gost/x/dialer/xhttp"

	// Register handlers
	_ "github.com/go-gost/x/handler/api"
	_ "github.com/go-gost/x/handler/auto"
	_ "github.com/go-gost/x/handler/dns"
	_ "github.com/go-gost/x/handler/file"
	_ "github.com/go-gost/x/handler/forward/local"
	_ "github.com/go-gost/x/handler/forward/remote"
	_ "github.com/go-gost/x/handler/http"
	_ "github.com/go-gost/x/handler/http2"
	_ "github.com/go-gost/x/handler/http3"
	_ "github.com/go-gost/x/handler/masque"
	_ "github.com/go-gost/x/handler/metrics"
	_ "github.com/go-gost/x/handler/relay"
	_ "github.com/go-gost/x/handler/router"
	_ "github.com/go-gost/x/handler/sni"
	_ "github.com/go-gost/x/handler/socks/v4"
	_ "github.com/go-gost/x/handler/socks/v5"
	_ "github.com/go-gost/x/handler/ss"
	_ "github.com/go-gost/x/handler/ss/udp"
	_ "github.com/go-gost/x/handler/sshd"
	_ "github.com/go-gost/x/handler/tunnel"
	_ "github.com/go-gost/x/handler/unix"

	// Register listeners
	_ "github.com/go-gost/x/listener/dns"
	_ "github.com/go-gost/x/listener/dtls"
	_ "github.com/go-gost/x/listener/ftcp"
	_ "github.com/go-gost/x/listener/grpc"
	_ "github.com/go-gost/x/listener/http2"
	_ "github.com/go-gost/x/listener/http2/h2"
	_ "github.com/go-gost/x/listener/http3"
	_ "github.com/go-gost/x/listener/http3/h3"
	_ "github.com/go-gost/x/listener/http3/wt"
	_ "github.com/go-gost/x/listener/kcp"
	_ "github.com/go-gost/x/listener/mtcp"
	_ "github.com/go-gost/x/listener/mtls"
	_ "github.com/go-gost/x/listener/mws"
	_ "github.com/go-gost/x/listener/obfs/http"
	_ "github.com/go-gost/x/listener/obfs/tls"
	_ "github.com/go-gost/x/listener/pht"
	_ "github.com/go-gost/x/listener/quic"
	_ "github.com/go-gost/x/listener/rtcp"
	_ "github.com/go-gost/x/listener/rudp"
	_ "github.com/go-gost/x/listener/runix"
	_ "github.com/go-gost/x/listener/ssh"
	_ "github.com/go-gost/x/listener/sshd"
	_ "github.com/go-gost/x/listener/tcp"
	_ "github.com/go-gost/x/listener/tls"
	_ "github.com/go-gost/x/listener/udp"
	_ "github.com/go-gost/x/listener/unix"
	_ "github.com/go-gost/x/listener/ws"
	_ "github.com/go-gost/x/listener/xhttp"
)
