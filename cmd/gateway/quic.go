// cmd/gateway/quic.go 启动可选的 QUIC 监听器，并把 QUIC 流适配到普通网关连接流程。

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/rs/zerolog/log"
)

type (
	// quicConn 把 QUIC connection 和单条 stream 组合成 net.Conn 风格对象，
	// 使后续转发逻辑不用区分 TCP 与 QUIC。
	quicConn struct {
		quic.Connection
		quic.Stream
	}
)

func runQuic(wg *sync.WaitGroup) {
	if wg != nil {
		defer wg.Done()
	}

	// QUIC 基于 UDP 监听，端口来自运行态服务配置。
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: config.Quic.Port})
	if err != nil {
		log.Panic().Err(err).Msg("Failed to listen UDP")
	}
	defer udpConn.Close()

	tlsConf, err := generateTLSConfig()
	if err != nil {
		log.Panic().Err(err).Msg("Failed to generate TLS config")
	}

	// quic-go 的 listener 接收 connection，真正的字节流在 stream 中。
	ln, err := quic.Listen(udpConn, tlsConf, nil)
	if err != nil {
		log.Panic().Err(err).Msg("Failed to listen QUIC")
	}
	log.Info().Int("port", config.Quic.Port).Msg("Listening for QUIC connections")

	for {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			log.Err(err).Msg("Error accepting QUIC connection")
			continue
		}

		log.Debug().
			Str("client", conn.RemoteAddr().String()).
			Msg("Accepted QUIC connection")
		go handleQuicRequest(conn)
	}
}

func upstreamQuic(host string) net.Conn {
	tlsConf := &tls.Config{
		// 网关自管的 QUIC 上游默认使用临时证书，当前先跳过证书校验。
		InsecureSkipVerify: true, // 跳过证书检查
		NextProtos:         getQuicNextProtos(),
	}

	// 上游握手使用短超时，避免连接协程在不可达上游上长期等待。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, host, tlsConf, nil)
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Str("host", host).Msg("Failed to dial QUIC")
		return nil
	}

	stream, err := conn.OpenStream()
	if err != nil {
		gatewayMetrics.UpstreamDialError()
		log.Err(err).Str("host", host).Msg("Failed to open stream")
		conn.CloseWithError(0, "failed to open stream")
		return nil
	}
	log.Debug().Str("host", host).Msg("QUIC stream opened")

	// 返回的 quicConn 后续会收到 Minecraft 首包回放并进入普通双向转发。
	return quicConn{
		Connection: conn,
		Stream:     stream,
	}
}

func handleQuicRequest(conn quic.Connection) {
	defer conn.CloseWithError(0, "Closing connection")

	// 入口连接只等待第一条 stream；该 stream 承载完整 Minecraft 字节流。
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		log.Err(err).Msg("Error accepting stream")
		return
	}

	handleRequest(quicConn{
		Connection: conn,
		Stream:     stream,
	})
}

func generateTLSConfig() (*tls.Config, error) {
	// 生成临时私钥；当前 QUIC 入口不依赖磁盘证书文件。
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	// 创建自签证书模板，满足 QUIC TLS 握手要求。
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Example Org"},
		},
		NotBefore: time.Now(),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour), // 有效期 1 年。

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	// 自签名证书用于当前进程生命周期内的 QUIC 监听。
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, err
	}

	// 编码证书和私钥，再交给 tls.X509KeyPair 解析为标准证书结构。
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	keyPEMBlock := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyPEM})

	// 加载到 tls.Certificate。
	cert, err := tls.X509KeyPair(certPEM, keyPEMBlock)
	if err != nil {
		return nil, err
	}

	// 返回 QUIC listener 使用的 TLS 配置。
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   getQuicNextProtos(),
	}, nil
}

func getQuicNextProtos() []string {
	nextProtos := config.Quic.ApplicationProtocols
	if len(nextProtos) == 0 {
		return []string{"minecraft", "quic", "raw", "h3"} // 默认协议列表。
	}
	return nextProtos
}

func (c quicConn) Close() error {
	_ = c.Stream.Close()
	return c.Connection.CloseWithError(0, "Closing QUIC connection")
}

func (c quicConn) CloseWrite() error {
	// QUIC stream 关闭写方向即可通知对端没有更多数据。
	return c.Stream.Close()
}

func (c quicConn) CloseRead() error {
	// CancelRead 用于停止接收方向，匹配 relay.go 中的半关闭调用。
	c.Stream.CancelRead(0)
	return nil
}
